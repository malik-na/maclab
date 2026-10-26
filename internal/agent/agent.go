// Package agent is lab-agent: it runs on each test Mac, heartbeats to labd,
// and executes the commands labd sends.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/iconidentify/maclab/internal/api"
	"github.com/iconidentify/maclab/internal/detect"
)

const Version = "0.7.3"

type Config struct {
	Server   string `json:"server"`
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
	Secret   string `json:"secret"`
	GUIUser  string `json:"gui_user,omitempty"`
	WorkDir  string `json:"work_dir,omitempty"`
	// Enrollment is what labd said the Mac is, at setup.
	Enrollment api.AgentEnrollment `json:"enrollment"`
	// HTTPTimeoutSec bounds a check-in or report request, TransferTimeoutSec an
	// upload; a Mac far from labd needs both longer. Zero is the default.
	HTTPTimeoutSec     int `json:"http_timeout_sec,omitempty"`
	TransferTimeoutSec int `json:"transfer_timeout_sec,omitempty"`
	// Clock is where lab-agent reads time; nil is the wall clock.
	Clock Clock `json:"-"`
}

func (c *Config) workDir() string {
	if c.WorkDir != "" {
		return c.WorkDir
	}
	return "/var/lib/maclab"
}

func (c *Config) clock() Clock {
	if c.Clock == nil {
		return WallClock
	}
	return c.Clock
}

func (c *Config) httpTimeout() time.Duration {
	if c.HTTPTimeoutSec > 0 {
		return time.Duration(c.HTTPTimeoutSec) * time.Second
	}
	return 2 * time.Minute
}

func (c *Config) transferTimeout() time.Duration {
	if c.TransferTimeoutSec > 0 {
		return time.Duration(c.TransferTimeoutSec) * time.Second
	}
	return 30 * time.Minute
}

func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) Save(path string) error {
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

type Agent struct {
	cfg  *Config
	sys  System
	http *http.Client
	log  *log.Logger

	repMu             sync.Mutex // one report in flight at a time
	mu                sync.Mutex
	seen              map[string]bool
	running           map[string]bool
	results           []api.CommandResult
	events            []api.KernelEvent
	factsRevision     uint64
	sentFactsRevision uint64
	lastFacts         time.Time
	after             []func() // actions to run once pending results reach labd
	contacted         string   // the Lab boot whose contact with labd is on record
	kind              string   // the Mac's kind, as labd's check-ins say
	wake              chan struct{}
	life              context.Context // commands outlive the poll that delivered them
}

func New(cfg *Config, sys System, logger *log.Logger) *Agent {
	if logger == nil {
		logger = log.New(os.Stderr, "lab-agent: ", log.LstdFlags)
	}
	return &Agent{
		cfg:     cfg,
		sys:     sys,
		http:    &http.Client{Timeout: cfg.httpTimeout()},
		log:     logger,
		seen:    map[string]bool{},
		running: map[string]bool{},
		wake:    make(chan struct{}, 1),
	}
}

func (a *Agent) clock() Clock { return a.cfg.clock() }

// Enroll trades a one-time token for a device identity.
func Enroll(ctx context.Context, server, token string, facts api.Facts) (*api.EnrollResponse, error) {
	var resp api.EnrollResponse
	err := postJSON(ctx, http.DefaultClient, strings.TrimRight(server, "/")+"/api/agent/enroll", "", api.EnrollRequest{Token: token, Facts: facts}, &resp)
	return &resp, err
}

// Run heartbeats until ctx ends. The heartbeat is a long poll: labd holds it
// until it has a command for this Mac, so commands arrive at once. Results and
// kernel events go out on their own request the moment they exist.
func (a *Agent) Run(ctx context.Context) error {
	a.life = ctx
	go a.sys.WatchKernel(ctx, func(line string) {
		if k := detect.Classify(line); k != "" {
			bootID, _, _ := a.sys.Identity()
			a.mu.Lock()
			if len(a.events) < 500 {
				a.events = append(a.events, api.KernelEvent{Time: a.clock().Now(), Kind: k, Source: "kmsg", BootID: bootID, Line: line})
			}
			a.mu.Unlock()
			a.poke()
		}
	})
	go a.sys.WatchSleep(ctx, a.sleeping)
	go a.reportLoop(ctx)
	var pollMu sync.Mutex
	var pollCancel context.CancelFunc
	if s, ok := a.sys.(*Sim); ok {
		s.OnBoot = func() {
			pollMu.Lock()
			if pollCancel != nil {
				pollCancel()
			}
			pollMu.Unlock()
		}
		s.setEarlyUnit(func(ctx context.Context) {
			if err := enforceLabBootDeadlines(ctx, a.clock(), a.labBootDir(), s, a.log.Printf); err != nil {
				a.log.Printf("Lab boot deadlines: %v", err)
			}
		})
	}
	for ctx.Err() == nil {
		if !a.sys.Alive() {
			sleepCtx(ctx, a.clock(), 100*time.Millisecond)
			continue
		}
		pctx, cancel := context.WithCancel(ctx)
		pollMu.Lock()
		pollCancel = cancel
		pollMu.Unlock()
		err := a.poll(pctx)
		cancel()
		if pctx.Err() != nil && ctx.Err() == nil {
			continue // the machine rebooted under the long poll
		}
		if err != nil && ctx.Err() == nil {
			a.log.Printf("checkin: %v", err)
			sleepCtx(ctx, a.clock(), 5*time.Second)
		}
	}
	return ctx.Err()
}

// sleeping tells labd the Mac is about to suspend, before it does, and that
// it has resumed. Anything still running (a test that suspends) carries on.
func (a *Agent) sleeping(asleep bool) {
	bootID, _, _ := a.sys.Identity()
	e := api.KernelEvent{Time: a.clock().Now(), Kind: detect.Wake, Source: "logind", BootID: bootID, Line: "resumed from sleep"}
	if asleep {
		e.Kind, e.Line = detect.Sleep, "going to sleep"
	}
	a.mu.Lock()
	a.events = append(a.events, e)
	a.mu.Unlock()
	a.log.Printf("%s", e.Line)
	if !asleep {
		a.poke() // the network may still be down; reportLoop keeps retrying
		return
	}
	ctx, cancel := context.WithTimeout(a.life, 4*time.Second)
	defer cancel()
	if err := a.report(ctx); err != nil {
		a.log.Printf("could not tell labd before sleeping: %v", err)
	}
}

func sleepCtx(ctx context.Context, c Clock, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-c.After(d):
	}
}

func (a *Agent) poke() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

func (a *Agent) poll(ctx context.Context) error {
	bootID, kernel, cmdline := a.sys.Identity()
	ci := api.Checkin{BootID: bootID, Kernel: kernel, Cmdline: cmdline, Health: a.sys.Health(),
		SnapshotEligible: a.sys.SnapshotEligible() == nil}
	a.mu.Lock()
	factsRevision := a.factsRevision
	refreshFacts := a.lastFacts.IsZero() || a.sentFactsRevision != factsRevision || a.clock().Now().Sub(a.lastFacts) >= time.Minute
	for id := range a.running {
		ci.Running = append(ci.Running, id)
	}
	a.mu.Unlock()
	// A Volunteer's Mac with no settings sends empty ones, so labd's copy
	// never outlives a deleted file. One that can't be read sends none: the
	// agent refuses lab commands while it can't read them anyway.
	if s, set, err := loadWindow(a.cfg); err != nil {
		a.log.Printf("Availability window: %v", err)
	} else if set || a.volunteer() {
		ci.Window = &s
	}
	if refreshFacts {
		f := a.sys.Facts()
		f.AgentVersion = Version
		ci.Facts = &f
	}

	// A Lab boot's first check-in doesn't wait: its no-contact deadline lifts
	// only once the answer is back.
	wait := 10000
	a.mu.Lock()
	if IsLabBoot(cmdline) && a.contacted != bootID {
		wait = 0
	}
	a.mu.Unlock()
	var resp api.CheckinResponse
	if err := postJSON(ctx, a.http, a.url(fmt.Sprintf("/api/agent/checkin?wait_ms=%d", wait)), a.cfg.Secret, ci, &resp); err != nil {
		return err
	}
	a.reachedLabd(bootID, cmdline)
	a.mu.Lock()
	a.kind = resp.Kind
	if ci.Facts != nil {
		// Cleanup can finish while this request is in flight. Acknowledge
		// only the revision sent, so that refresh is not lost.
		a.sentFactsRevision = factsRevision
		a.lastFacts = a.clock().Now()
	}
	a.mu.Unlock()
	for _, c := range resp.Commands {
		a.start(a.life, c)
	}
	return nil
}

func (a *Agent) reportLoop(ctx context.Context) {
	retry := a.clock().NewTicker(3 * time.Second)
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.wake:
		case <-retry.C():
		}
		if a.sys.Alive() {
			if err := a.report(ctx); err != nil && ctx.Err() == nil {
				a.log.Printf("report: %v", err)
			}
		}
	}
}

// report sends finished results and new kernel events, then runs any action
// (a reboot) that had to wait until its result reached labd.
func (a *Agent) report(ctx context.Context) error {
	a.repMu.Lock()
	defer a.repMu.Unlock()
	a.mu.Lock()
	rep := api.Report{Results: append([]api.CommandResult(nil), a.results...), Events: append([]api.KernelEvent(nil), a.events...)}
	a.mu.Unlock()
	if len(rep.Results) == 0 && len(rep.Events) == 0 {
		return nil
	}
	if err := postJSON(ctx, a.http, a.url("/api/agent/report"), a.cfg.Secret, rep, nil); err != nil {
		return err
	}
	a.mu.Lock()
	a.results = a.results[len(rep.Results):]
	a.events = a.events[len(rep.Events):]
	var after []func()
	if len(a.results) == 0 {
		after, a.after = a.after, nil
	}
	a.mu.Unlock()
	for _, f := range after {
		f()
	}
	return nil
}

func (a *Agent) start(ctx context.Context, c api.Command) {
	a.mu.Lock()
	if a.seen[c.ID] {
		a.mu.Unlock()
		return
	}
	a.seen[c.ID] = true
	a.running[c.ID] = true
	a.mu.Unlock()
	a.log.Printf("command %s %s job=%s", c.ID, c.Kind, c.JobID)
	go func() {
		out, after, err := a.exec(ctx, c)
		r := api.CommandResult{ID: c.ID, OK: err == nil}
		if err != nil {
			r.Error = err.Error()
			a.log.Printf("command %s failed: %v", c.ID, err)
		}
		if out != nil {
			r.Output, _ = json.Marshal(out)
		}
		a.mu.Lock()
		delete(a.running, c.ID)
		a.results = append(a.results, r)
		if after != nil && err == nil {
			a.after = append(a.after, after)
		}
		a.mu.Unlock()
		a.poke()
	}()
}

// exec runs one command. The returned func runs only after the result reaches labd,
// so a reboot never swallows its own acknowledgement.
func (a *Agent) exec(ctx context.Context, c api.Command) (any, func(), error) {
	if err := a.refuseOutsideWindow(c); err != nil {
		return nil, nil, err
	}
	fetch := a.fetch
	switch c.Kind {
	case api.CmdStage:
		var args api.StageArgs
		if err := json.Unmarshal(c.Args, &args); err != nil {
			return nil, nil, err
		}
		res, err := a.sys.Stage(ctx, c.JobID, args, fetch)
		return res, nil, err
	case api.CmdBootOnce:
		var args api.BootOnceArgs
		if err := json.Unmarshal(c.Args, &args); err != nil {
			return nil, nil, err
		}
		if err := a.armLabBoot(args); err != nil {
			return nil, nil, err
		}
		if err := a.sys.BootOnce(args.Entry); err != nil {
			return nil, nil, err
		}
		return nil, a.rebootLater, nil
	case api.CmdReboot:
		return nil, a.rebootLater, nil
	case api.CmdCrash:
		var args api.CrashArgs
		if err := json.Unmarshal(c.Args, &args); err != nil {
			return nil, nil, err
		}
		return nil, func() {
			if err := a.sys.Crash(args.Mode); err != nil {
				a.log.Printf("crash: %v", err)
			}
		}, nil
	case api.CmdRunTest:
		var t api.TestSpec
		if err := json.Unmarshal(c.Args, &t); err != nil {
			return nil, nil, err
		}
		dir, err := a.outDir(c)
		if err != nil {
			return nil, nil, err
		}
		res := a.sys.RunTest(ctx, c.JobID, t, fetch, dir)
		res.Files = a.uploadDir(ctx, c.JobID, "test-"+t.Name, dir)
		return res, nil, nil
	case api.CmdCollect:
		var args api.CollectArgs
		if err := json.Unmarshal(c.Args, &args); err != nil {
			return nil, nil, err
		}
		dir, err := a.outDir(c)
		if err != nil {
			return nil, nil, err
		}
		if err := a.sys.Collect(ctx, args.Boot, dir); err != nil {
			return nil, nil, err
		}
		prefix := "boot0"
		if args.Boot == "-1" {
			prefix = "prevboot"
		}
		return a.uploadDir(ctx, c.JobID, prefix, dir), nil, nil
	case api.CmdCollectLost:
		dir, err := a.outDir(c)
		if err != nil {
			return nil, nil, err
		}
		return a.collectLost(ctx, c.JobID, dir), nil, nil
	case api.CmdCleanup:
		var args api.CleanupArgs
		_ = json.Unmarshal(c.Args, &args)
		err := a.sys.Cleanup(c.JobID, args.All)
		// Even a partial cleanup can change free space and preflight.
		a.mu.Lock()
		a.factsRevision++
		a.mu.Unlock()
		return nil, nil, err
	case api.CmdScreen:
		var args api.ScreenArgs
		_ = json.Unmarshal(c.Args, &args)
		return a.screenshot(ctx, c, args)
	case api.CmdConfig:
		cfg, err := a.sys.KernelConfig()
		return cfg, nil, err
	case api.CmdExec:
		var args api.ExecArgs
		_ = json.Unmarshal(c.Args, &args)
		return a.sys.Exec(ctx, args), nil, nil
	case api.CmdRecordKnownGood:
		var args api.RecordKnownGoodArgs
		if err := json.Unmarshal(c.Args, &args); err != nil {
			return nil, nil, err
		}
		err := a.sys.RecordKnownGood(args.Kernel)
		a.mu.Lock()
		a.factsRevision++ // preflight reads the record
		a.mu.Unlock()
		return nil, nil, err
	case api.CmdLogs:
		var args api.LogsArgs
		_ = json.Unmarshal(c.Args, &args)
		out, err := a.sys.Logs(ctx, args)
		return out, nil, err
	}
	return nil, nil, fmt.Errorf("unknown command %q", c.Kind)
}

func (a *Agent) screenshot(ctx context.Context, c api.Command, args api.ScreenArgs) (any, func(), error) {
	dir, err := a.outDir(c)
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(dir)
	wait := time.Duration(args.WaitSec) * time.Second
	if wait <= 0 {
		wait = 5 * time.Second
	}
	path := filepath.Join(dir, "screen.jpg")
	w, h, err := a.sys.Screenshot(ctx, path, wait)
	if err != nil {
		return nil, nil, err
	}
	res := api.ScreenResult{Time: a.clock().Now(), Width: w, Height: h}
	if c.JobID != "" {
		label := args.Label
		if label == "" {
			label = res.Time.Format("150405")
		}
		res.Name = "screen/" + label + ".jpg"
		err = a.upload(ctx, c.JobID, res.Name, path)
	} else {
		res.Name, err = a.uploadScreen(ctx, path)
	}
	return res, nil, err
}

func (a *Agent) uploadScreen(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	req, _ := http.NewRequestWithContext(ctx, "POST", a.url("/api/agent/screen"), f)
	req.Header.Set("Authorization", "Bearer "+a.cfg.Secret)
	req.Header.Set("Content-Type", "image/jpeg")
	resp, err := (&http.Client{Timeout: a.cfg.transferTimeout()}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("screen upload: %s", strings.TrimSpace(string(b)))
	}
	return strings.TrimSpace(string(b)), nil
}

func (a *Agent) labBootDir() string { return filepath.Join(a.workDir(), labBootDir) }

// armLabBoot writes the deadline record the early unit enforces on the Lab
// boot labd is about to start, and forgets the last one.
func (a *Agent) armLabBoot(args api.BootOnceArgs) error {
	if args.BootTimeoutSec <= 0 || args.JobSec <= 0 {
		return errors.New("a Lab boot needs its deadlines: boot_timeout_sec and job_sec")
	}
	now := a.clock().Now()
	// The ceiling is the job's end, or on a Volunteer's Mac the Availability
	// window's end if that comes first.
	jobEnd := a.capAtWindowEnd(now.Add(time.Duration(args.JobSec) * time.Second))
	d := newLabBootDeadlines(now, time.Duration(args.BootTimeoutSec)*time.Second, jobEnd)
	dir := a.labBootDir()
	for _, f := range []string{labBootContactFile, labBootSeenFile} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("forget the last Lab boot: %w", err)
		}
	}
	if err := writeLabBootDeadlines(filepath.Join(dir, labBootDeadlineFile), d); err != nil {
		return fmt.Errorf("deadline record: %w", err)
	}
	a.log.Printf("Lab boot deadlines: no contact %s, ceiling %s", d.NoContact.Format(time.RFC3339), d.Ceiling.Format(time.RFC3339))
	return nil
}

// reachedLabd records that a Lab boot has reached labd, which lifts its
// no-contact deadline.
func (a *Agent) reachedLabd(bootID, cmdline string) {
	a.mu.Lock()
	done := a.contacted == bootID
	a.mu.Unlock()
	if done || !IsLabBoot(cmdline) {
		return
	}
	if err := os.MkdirAll(a.labBootDir(), 0o755); err != nil {
		a.log.Printf("Lab boot contact: %v", err)
		return
	}
	if err := writeAtomic(filepath.Join(a.labBootDir(), labBootContactFile), []byte(bootID+"\n")); err != nil {
		a.log.Printf("Lab boot contact: %v", err)
		return
	}
	a.mu.Lock()
	a.contacted = bootID
	a.mu.Unlock()
}

// collectLost uploads what is left of the Lab boot the Mac came back from: its
// journal by boot ID and any pstore/ramoops records, and says whether the
// early unit rebooted it at a deadline.
func (a *Agent) collectLost(ctx context.Context, job, dir string) api.LostBoot {
	var lost api.LostBoot
	ld := a.labBootDir()
	d, _ := readLabBootDeadlines(filepath.Join(ld, labBootDeadlineFile)) // none: any previous boot will do
	var seen labBootSeen
	if b, err := os.ReadFile(filepath.Join(ld, labBootSeenFile)); err == nil {
		if err := json.Unmarshal(b, &seen); err != nil {
			a.log.Printf("lost boot: %s: %v", labBootSeenFile, err)
		}
	}
	lost.BootID, lost.Deadline = seen.BootID, seen.Deadline
	if lost.BootID == "" {
		// The early unit never ran: the Lab boot went down before userspace.
		lost.BootID = a.sys.PreviousBoot(d.Armed)
	}
	// Whatever is missing, labd still needs to hear how the boot ended.
	if lost.BootID != "" {
		if err := a.sys.Collect(ctx, lost.BootID, dir); err != nil {
			a.log.Printf("lost boot %s: journal: %v", lost.BootID, err)
		}
	}
	if _, err := a.sys.CollectPstore(d.Armed, filepath.Join(dir, "pstore")); err != nil {
		a.log.Printf("lost boot %s: pstore: %v", lost.BootID, err)
	}
	lost.Files = a.uploadDir(ctx, job, "prevboot", dir)
	return lost
}

func (a *Agent) rebootLater() {
	a.log.Printf("rebooting")
	if err := a.sys.Reboot(); err != nil {
		a.log.Printf("reboot: %v", err)
	}
}

func (a *Agent) workDir() string { return a.cfg.workDir() }

func (a *Agent) outDir(c api.Command) (string, error) {
	dir := filepath.Join(a.workDir(), "out", c.ID)
	os.RemoveAll(dir)
	return dir, os.MkdirAll(dir, 0o755)
}

func (a *Agent) url(p string) string { return strings.TrimRight(a.cfg.Server, "/") + p }

// fetch downloads an artifact to dst, resuming with a Range request when the
// transfer drops: over a slow link a long download rarely survives one try. The
// bytes so far wait in dst+".part" until the whole file matches its sha256.
func (a *Agent) fetch(ctx context.Context, sha, dst string) error {
	part := dst + ".part"
	var err error
	wait := time.Second
	for try := 0; ; try++ {
		var permanent bool
		if permanent, err = a.fetchOnce(ctx, sha, part); err == nil {
			return os.Rename(part, dst)
		}
		if permanent || ctx.Err() != nil || try >= 8 {
			return err
		}
		a.log.Printf("fetch %s: %v; resuming in %s", short(sha), err, wait)
		select {
		case <-ctx.Done():
			return err
		case <-a.clock().After(wait):
		}
		wait = min(wait*2, 30*time.Second)
	}
}

func short(s string) string { return s[:min(len(s), 12)] }

// fetchOnce continues the download in part. permanent means retrying can't help.
func (a *Agent) fetchOnce(ctx context.Context, sha, part string) (permanent bool, err error) {
	have := int64(0)
	if fi, err := os.Stat(part); err == nil {
		have = fi.Size()
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", a.url("/api/agent/artifacts/"+sha), nil)
	req.Header.Set("Authorization", "Bearer "+a.cfg.Secret)
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	flags := os.O_WRONLY | os.O_CREATE
	switch {
	case resp.StatusCode == http.StatusPartialContent:
		flags |= os.O_APPEND
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		// The partial file is as long as the artifact or longer: check it as it is.
		have = -1
	case resp.StatusCode == http.StatusOK:
		flags |= os.O_TRUNC // labd ignored the Range: start over
	default:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return resp.StatusCode < 500, fmt.Errorf("artifact %s: %s: %s", sha, resp.Status, strings.TrimSpace(string(b)))
	}
	if have >= 0 {
		f, err := os.OpenFile(part, flags, 0o644)
		if err != nil {
			return true, err
		}
		_, err = io.Copy(f, resp.Body)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return false, err
		}
	}
	got, err := fileSHA256(part)
	if err != nil {
		return true, err
	}
	if got != sha {
		os.Remove(part)
		// A bad prefix from an earlier attempt is gone now: one more go from the start.
		return have == 0, fmt.Errorf("artifact %s: downloaded sha256 %s", sha, got)
	}
	return false, nil
}

// uploadDir sends every file under dir to labd as prefix/<relpath> and returns the names.
func (a *Agent) uploadDir(ctx context.Context, job, prefix, dir string) []string {
	var names []string
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		name := prefix + "/" + filepath.ToSlash(rel)
		if err := a.upload(ctx, job, name, p); err != nil {
			a.log.Printf("upload %s: %v", name, err)
			return nil
		}
		names = append(names, name)
		return nil
	})
	os.RemoveAll(dir)
	return names
}

// upload sends one file, retrying for a few minutes: after a resume, Wi-Fi
// takes a while to come back and the first attempts fail.
func (a *Agent) upload(ctx context.Context, job, name, path string) error {
	var err error
	wait := 2 * time.Second
	for try := 0; ; try++ {
		if err = a.uploadOnce(ctx, job, name, path); err == nil || errors.Is(err, os.ErrNotExist) || errors.Is(err, errRefused) || try >= 8 {
			return err
		}
		a.log.Printf("upload %s: %v; retrying in %s", name, err, wait)
		select {
		case <-ctx.Done():
			return err
		case <-a.clock().After(wait):
		}
		wait = min(wait*2, 30*time.Second)
	}
}

func (a *Agent) uploadOnce(ctx context.Context, job, name, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	q := url.Values{"job": {job}, "name": {name}}
	req, _ := http.NewRequestWithContext(ctx, "POST", a.url("/api/agent/upload?"+q.Encode()), f)
	req.Header.Set("Authorization", "Bearer "+a.cfg.Secret)
	resp, err := (&http.Client{Timeout: a.cfg.transferTimeout()}).Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return fmt.Errorf("%w: %s", errRefused, resp.Status)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s", resp.Status)
	}
	return nil
}

// errRefused is labd turning a request down; trying again won't help.
var errRefused = errors.New("refused")

func postJSON(ctx context.Context, c *http.Client, u, secret string, in, out any) error {
	b, _ := json.Marshal(in)
	req, _ := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return doJSON(c, req, secret, out)
}

func getJSON(ctx context.Context, c *http.Client, u, secret string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	return doJSON(c, req, secret, out)
}

func doJSON(c *http.Client, req *http.Request, secret string, out any) error {
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}
