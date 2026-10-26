// Package server is labd: the controller that owns devices, leases, jobs
// and the recovery ladder. Agents, the lab CLI and the MCP bridge talk to it over HTTP.
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iconidentify/maclab/internal/agent"
	"github.com/iconidentify/maclab/internal/api"
	"github.com/iconidentify/maclab/internal/oob"
	"github.com/iconidentify/maclab/internal/web"
)

type Config struct {
	DataDir string
	// FirstMaintainer and FirstMaintainerToken name the Maintainer a labd
	// with none starts with: on upgrade, whoever held the old admin token.
	FirstMaintainer      string
	FirstMaintainerToken string
	Interval             time.Duration // agent heartbeat interval
	BootTimeout          time.Duration // default time for a Mac to come back after a reboot
	OfflineAfter         time.Duration
	// AliveWindow, QuietGrace and StageTimeout are the timeouts that assume a
	// LAN; a Mac far from labd needs them longer. Zero keeps the LAN default;
	// labd's flags default to remote-sized values.
	AliveWindow  time.Duration // how stale a heartbeat may be before a Mac counts unresponsive
	QuietGrace   time.Duration // silence on the same boot before a command counts lost
	StageTimeout time.Duration // time a Mac gets to stage a kernel, artifact download included
	// WatchdogOpenTimeout is watchdog.open_timeout on every Lab boot's command
	// line; 0 is lab-agent's default (150 s).
	WatchdogOpenTimeout time.Duration
	NtfyURL             string
	NotifyCmd           string
	// OOBFactory builds a controller for a device; tests swap in fakes.
	OOBFactory func(*api.Device) (oob.Controller, error)
	// BuilderToken authenticates builder hosts; empty disables builds.
	BuilderToken string
	// ArtifactBudget caps the artifact store; unreferenced artifacts go first.
	ArtifactBudget int64
	// OMT runs omarchy-m-test in every job on Macs with a desktop user.
	OMT bool
	// OMTSite is where omarchy-m-test reports are published; empty never publishes.
	OMTSite string
	// OMTPublish publishes known-good runs (baselines and scheduled runs) by itself.
	OMTPublish bool
	// OMTEvery re-runs omarchy-m-test on each idle Mac's known-good kernel this often; 0 never.
	OMTEvery time.Duration
	// Clock is where labd reads time; nil is the wall clock.
	Clock agent.Clock
}

type Server struct {
	cfg   Config
	store *store
	log   *log.Logger
	ctx   context.Context
	start time.Time

	mu          sync.Mutex
	devs        map[string]*dev // by name
	cancels     map[string]context.CancelFunc
	hub         hub
	builds      *buildMgr
	notifyHooks []func(Notification)
	conns       maintainerConns
	serialMu    sync.Mutex
}

// clock is where labd reads time: Config.Clock, or the wall clock when unset.
func (s *Server) clock() agent.Clock {
	if s.cfg.Clock == nil {
		return agent.WallClock
	}
	return s.cfg.Clock
}

func (s *Server) now() time.Time                  { return s.clock().Now() }
func (s *Server) since(t time.Time) time.Duration { return s.clock().Now().Sub(t) }

// withTimeout is context.WithTimeout on labd's clock.
func (s *Server) withTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	c := s.clock()
	if c == agent.WallClock {
		return context.WithTimeout(parent, d)
	}
	ctx, cancel := context.WithCancel(parent)
	go func() {
		select {
		case <-ctx.Done():
		case <-c.After(d):
			cancel()
		}
	}()
	return ctx, cancel
}

func New(ctx context.Context, cfg Config, logger *log.Logger) (*Server, error) {
	if logger == nil {
		logger = log.New(os.Stderr, "labd: ", log.LstdFlags)
	}
	if cfg.Interval == 0 {
		cfg.Interval = 8 * time.Second
	}
	if cfg.BootTimeout == 0 {
		cfg.BootTimeout = 4 * time.Minute
	}
	if cfg.OfflineAfter == 0 {
		cfg.OfflineAfter = time.Minute
	}
	if cfg.StageTimeout == 0 {
		cfg.StageTimeout = 20 * time.Minute
	}
	if cfg.OOBFactory == nil {
		cfg.OOBFactory = func(d *api.Device) (oob.Controller, error) { return oob.New(d.OOB) }
	}
	for _, d := range []string{"artifacts", "jobs", "serial", "baselines", "builds"} {
		if err := os.MkdirAll(filepath.Join(cfg.DataDir, d), 0o755); err != nil {
			return nil, err
		}
	}
	st, err := openStore(filepath.Join(cfg.DataDir, "labd.db"))
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, store: st, log: logger, ctx: ctx, devs: map[string]*dev{}, cancels: map[string]context.CancelFunc{}}
	s.start = s.now()
	if err := s.firstMaintainer(); err != nil {
		return nil, err
	}
	rows, err := st.devices()
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		d := row.d
		d.ActiveJob = ""
		if d.Kind == "" {
			d.Kind = api.KindLabOwned // enrolled before Volunteer Macs existed
		}
		if d.State == api.StateBusy || d.State == api.StateRecovering {
			d.State = api.StateOffline
		}
		v := newDev(s, d, row.secretHash)
		v.dirty = true // labd may have stopped mid-job
		s.devs[d.Name] = v
		if l := s.screens(d.Name); len(l) > 0 {
			v.screenAt, v.screenName = l[0].Time, l[0].Name
		}
		s.attachOOB(v)
		go s.deviceLoop(v)
	}
	// Jobs that were running when labd stopped cannot be resumed.
	unfinished, _ := st.jobs("", 1000, true)
	sort.Slice(unfinished, func(i, k int) bool { return unfinished[i].Created.Before(unfinished[k].Created) })
	for _, j := range unfinished {
		if j.State == api.JobQueued {
			if v := s.devs[j.Spec.Device]; v != nil {
				v.queue <- j.ID
				continue
			}
		}
		j.State, j.Outcome, j.Summary = api.JobDone, api.OutcomeInfra, "labd restarted while the job was running"
		st.saveJob(j)
	}
	s.builds = newBuildMgr(s)
	go s.monitor()
	go s.buildMonitor()
	go s.gcLoop()
	go s.omtScheduler()
	return s, nil
}

// OnNotify registers a hook, used by tests to observe notifications.
func (s *Server) OnNotify(f func(Notification)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifyHooks = append(s.notifyHooks, f)
}

func (s *Server) monitor() {
	t := s.clock().NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C():
		}
		for _, v := range s.allDevs() {
			v.mu.Lock()
			// After a restart, agents need a moment to reconnect before silence means anything.
			idle := (v.d.State == api.StateReady || v.d.State == api.StateNew) && s.since(s.start) > s.cfg.OfflineAfter
			if idle && !v.d.LastSeen.IsZero() && s.since(v.d.LastSeen) > s.cfg.OfflineAfter {
				v.restore = v.d.State
				why := "no heartbeat since " + v.d.LastSeen.Format(time.TimeOnly)
				if !v.asleep.IsZero() {
					why = "asleep since " + v.asleep.Format(time.TimeOnly) + "; a key press wakes it"
				}
				v.setStateLocked(api.StateOffline, why)
			}
			v.mu.Unlock()
		}
	}
}

func (s *Server) allDevs() []*dev {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*dev, 0, len(s.devs))
	for _, v := range s.devs {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].d.Name < out[j].d.Name })
	return out
}

func (s *Server) dev(name string) *dev {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.devs[name]
}

// attachOOB (re)connects a device's out-of-band controller and serial stream.
func (s *Server) attachOOB(v *dev) {
	v.mu.Lock()
	if v.oobStop != nil {
		v.oobStop()
		v.oobStop, v.oob, v.serialUp = nil, nil, false
	}
	cfg := v.d.OOB
	v.mu.Unlock()
	if cfg == nil {
		return
	}
	d := v.snapshot()
	ctl, err := s.cfg.OOBFactory(&d)
	if err != nil {
		s.log.Printf("%s: oob: %v", d.Name, err)
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	v.mu.Lock()
	v.oob, v.oobStop = ctl, cancel
	v.mu.Unlock()
	go func() {
		backoff := time.Second
		for ctx.Err() == nil {
			v.mu.Lock()
			v.serialUp = true
			v.mu.Unlock()
			err := ctl.Serial(ctx, func(line string) {
				backoff = time.Second
				if strings.TrimSpace(line) == "" {
					return // oobd keepalive
				}
				s.appendSerial(d.Name, line)
				v.onSerial(line)
			})
			v.mu.Lock()
			v.serialUp = false
			v.mu.Unlock()
			if ctx.Err() != nil {
				return
			}
			s.log.Printf("%s: serial stream: %v (retrying in %s)", d.Name, err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-s.clock().After(backoff):
			}
			if backoff < time.Minute {
				backoff *= 2
			}
		}
	}()
}

func (s *Server) serialPath(name string) string {
	return filepath.Join(s.cfg.DataDir, "serial", name+".log")
}

func (s *Server) appendSerial(name, line string) {
	s.serialMu.Lock()
	defer s.serialMu.Unlock()
	f, err := os.OpenFile(s.serialPath(name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	fmt.Fprintf(f, "%s %s\n", s.now().Format("2006-01-02T15:04:05.000"), line)
	f.Close()
}

func (s *Server) serialOffset(name string) int64 {
	st, err := os.Stat(s.serialPath(name))
	if err != nil {
		return 0
	}
	return st.Size()
}

// copySerial saves the serial console captured during a job alongside its logs.
func (s *Server) copySerial(name string, from int64, job string) {
	s.serialMu.Lock()
	defer s.serialMu.Unlock()
	f, err := os.Open(s.serialPath(name))
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return
	}
	b, _ := io.ReadAll(f)
	if len(b) == 0 {
		return
	}
	os.MkdirAll(s.jobDir(job), 0o755)
	os.WriteFile(filepath.Join(s.jobDir(job), "serial.log"), b, 0o644)
	s.updateJobByID(job, func(j *api.Job) { j.Result.Logs = append(j.Result.Logs, "serial.log") })
}

func (s *Server) jobDir(id string) string { return filepath.Join(s.cfg.DataDir, "jobs", id) }
func (s *Server) baselinePath(name string) string {
	return filepath.Join(s.cfg.DataDir, "baselines", name+"-dmesg-errors.txt")
}

var jobMu sync.Mutex

func (s *Server) updateJob(j *api.Job, f func(*api.Job)) {
	jobMu.Lock()
	defer jobMu.Unlock()
	f(j)
	j.Updated = s.now()
	if err := s.store.saveJob(j); err != nil {
		s.log.Printf("save job %s: %v", j.ID, err)
	}
	s.hub.publish("job", j.Spec.Device, map[string]any{"id": j.ID, "device": j.Spec.Device, "state": j.State, "outcome": j.Outcome})
}

func (s *Server) updateJobByID(id string, f func(*api.Job)) {
	jobMu.Lock()
	j, err := s.store.job(id)
	jobMu.Unlock()
	if err == nil {
		s.updateJob(j, f)
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func hashSecret(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// ---- HTTP ----

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /api/agent/enroll", s.hEnroll)
	m.HandleFunc("POST /api/agent/checkin", s.agent(s.hCheckin))
	m.HandleFunc("POST /api/agent/report", s.agent(s.hReport))
	m.HandleFunc("GET /api/agent/enrollment", s.agent(s.hAgentEnrollment))
	m.HandleFunc("GET /api/agent/artifacts/{sha}", s.agent(s.hAgentArtifact))
	m.HandleFunc("POST /api/agent/upload", s.agent(s.hUpload))

	m.HandleFunc("GET /api/devices", s.maintainer(s.hDevices))
	m.HandleFunc("GET /api/devices/{name}", s.maintainer(s.hDevice))
	m.HandleFunc("PATCH /api/devices/{name}", s.maintainer(s.hPatchDevice))
	m.HandleFunc("POST /api/devices/{name}/lease", s.maintainer(s.hLease))
	m.HandleFunc("DELETE /api/devices/{name}/lease", s.maintainer(s.hRelease))
	m.HandleFunc("POST /api/devices/{name}/reset", s.maintainer(s.hReset))
	m.HandleFunc("GET /api/devices/{name}/serial", s.maintainer(s.hSerial))
	m.HandleFunc("GET /api/devices/{name}/oob", s.maintainer(s.hOOBStatus))
	m.HandleFunc("GET /api/stream", s.maintainer(s.hStream))
	m.HandleFunc("POST /api/devices/{name}/screenshot", s.maintainer(s.hScreenshot))
	m.HandleFunc("GET /api/devices/{name}/screen", s.maintainer(s.hScreen))
	m.HandleFunc("GET /api/devices/{name}/screens", s.maintainer(s.hScreens))
	m.HandleFunc("GET /api/devices/{name}/screens/{file}", s.maintainer(s.hScreen))
	m.HandleFunc("GET /api/devices/{name}/logs", s.maintainer(s.hLogs))
	m.HandleFunc("GET /api/devices/{name}/console", s.maintainer(s.hConsoleRaw))
	m.HandleFunc("POST /api/devices/{name}/console", s.maintainer(s.hConsoleWrite))
	m.HandleFunc("POST /api/devices/{name}/console/arm", s.maintainer(s.hConsoleArm))
	m.HandleFunc("POST /api/agent/screen", s.agent(s.hAgentScreen))
	m.HandleFunc("POST /api/enroll-tokens", s.maintainer(s.hEnrollToken))
	m.HandleFunc("POST /api/artifacts", s.maintainer(s.hPutArtifact))
	m.HandleFunc("POST /api/jobs", s.maintainer(s.hSubmit))
	m.HandleFunc("GET /api/jobs", s.maintainer(s.hJobs))
	m.HandleFunc("GET /api/jobs/{id}", s.maintainer(s.hJob))
	m.HandleFunc("GET /api/jobs/{id}/wait", s.maintainer(s.hWait))
	m.HandleFunc("POST /api/jobs/{id}/cancel", s.maintainer(s.hCancel))
	m.HandleFunc("POST /api/jobs/{id}/publish", s.maintainer(s.hPublish))
	m.HandleFunc("GET /api/jobs/{id}/files/{name...}", s.maintainer(s.hJobFile))
	m.HandleFunc("POST /api/builder/poll", s.builderAuth(s.hBuilderPoll))
	m.HandleFunc("POST /api/builder/builds/{id}/progress", s.builderAuth(s.hBuilderProgress))
	m.HandleFunc("POST /api/builder/builds/{id}/done", s.builderAuth(s.hBuilderDone))
	m.HandleFunc("POST /api/builder/artifacts", s.builderAuth(s.hBuilderArtifact))
	m.HandleFunc("GET /api/builder/artifacts/{sha}", s.builderAuth(s.hAgentArtifact))
	m.HandleFunc("POST /api/builds", s.maintainer(s.hCreateBuild))
	m.HandleFunc("POST /api/installed/{release}/manifest", s.maintainer(s.hFreezeManifest))
	m.HandleFunc("POST /api/installed/{release}/entry/{device}", s.maintainer(s.hFreezeEntry))
	m.HandleFunc("POST /api/installed/{release}/promote", s.maintainer(s.hPromote))
	m.HandleFunc("GET /api/installed/{release}", s.maintainer(s.hInstalledStatus))
	m.HandleFunc("GET /api/builds", s.maintainer(s.hBuilds))
	m.HandleFunc("GET /api/builds/{id}", s.maintainer(s.hBuild))
	m.HandleFunc("GET /api/builds/{id}/log", s.maintainer(s.hBuildLog))
	m.HandleFunc("GET /api/builds/{id}/files/{name}", s.maintainer(s.hBuildFile))
	m.HandleFunc("POST /api/builds/{id}/cancel", s.maintainer(s.hCancelBuild))
	m.HandleFunc("GET /api/builders", s.maintainer(s.hBuilders))
	m.HandleFunc("POST /api/devices/{name}/exec", s.maintainer(s.hExec))
	m.HandleFunc("GET /api/maintainers", s.maintainer(s.hMaintainers))
	m.HandleFunc("POST /api/maintainers", s.maintainer(s.hAddMaintainer))
	m.HandleFunc("DELETE /api/maintainers/{name}", s.maintainer(s.hRemoveMaintainer))
	web.Register(m)
	return m
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, code int, format string, args ...any) {
	http.Error(w, fmt.Sprintf(format, args...), code)
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

type ctxKey struct{}

func (s *Server) agent(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hash := hashSecret(bearer(r))
		for _, v := range s.allDevs() {
			v.mu.Lock()
			ok := subtle.ConstantTimeCompare([]byte(hash), []byte(v.secretHash)) == 1
			v.mu.Unlock()
			if ok {
				h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, v)))
				return
			}
		}
		httpErr(w, 401, "unknown device secret")
	}
}

func agentDev(r *http.Request) *dev { return r.Context().Value(ctxKey{}).(*dev) }

var reName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

func (s *Server) hEnrollToken(w http.ResponseWriter, r *http.Request) {
	var req api.EnrollTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	if !reName.MatchString(req.Name) {
		httpErr(w, 400, "device name must be lowercase letters, digits and dashes")
		return
	}
	// A Mac that never checked in hasn't taken the name: its enrollment failed,
	// and enrolling again with the new token replaces its secret.
	if v := s.dev(req.Name); v != nil && v.checkedIn() {
		httpErr(w, 409, "device %s already enrolled", req.Name)
		return
	}
	if req.Model == "" {
		req.Model = api.ModelLaptop
	}
	req.Volunteer, req.Contact = strings.TrimSpace(req.Volunteer), strings.TrimSpace(req.Contact)
	if (req.Volunteer == "") != (req.Contact == "") {
		httpErr(w, 400, "a Volunteer's token needs both the Volunteer's name and their contact")
		return
	}
	tok := "enr_" + randHex(16)
	exp := s.now().Add(24 * time.Hour)
	if err := s.store.addToken(hashSecret(tok), req, exp); err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	writeJSON(w, api.EnrollTokenResponse{Token: tok, Expires: exp})
}

func (s *Server) tokenValid(hash string) bool {
	_, err := s.store.token(hash, s.now())
	return err == nil
}

func (s *Server) hEnroll(w http.ResponseWriter, r *http.Request) {
	var req api.EnrollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	t, err := s.store.token(hashSecret(req.Token), s.now())
	if err != nil {
		httpErr(w, 403, "enrollment token unknown, used or expired")
		return
	}
	secret := "dev_" + randHex(24)
	// The token is spent by the Mac's first check-in, not by this call: if the
	// answer never reached the Mac, enrolling again must work. Until then the
	// Mac it made gets a new secret.
	if v := s.dev(t.Name); v != nil {
		v.mu.Lock()
		// Its own token again, or a new one once the old one has run out.
		retry := v.d.BootID == "" && (v.enrolledWith == hashSecret(req.Token) || !s.tokenValid(v.enrolledWith))
		if retry {
			v.enrolledWith = hashSecret(req.Token)
			v.secretHash = hashSecret(secret)
			v.d.Enrollment = enrollment(t, v.d.Enrollment)
			v.saveLocked()
		}
		d := v.d
		v.mu.Unlock()
		if !retry {
			httpErr(w, 409, "device %s already enrolled", t.Name)
			return
		}
		s.log.Printf("enrolled %s again before its first check-in", t.Name)
		writeJSON(w, api.EnrollResponse{DeviceID: d.ID, Name: d.Name, Secret: secret, AgentEnrollment: s.agentEnrollment(d.Enrollment)})
		return
	}
	d := api.Device{ID: "d-" + randHex(6), Name: t.Name, Label: t.Label, Model: t.Model, State: api.StateNew,
		StateReason: "enrolled; run a baseline job", Facts: req.Facts, Enrollment: enrollment(t, api.Enrollment{})}
	v := newDev(s, d, hashSecret(secret))
	v.enrolledWith = hashSecret(req.Token)
	s.mu.Lock()
	if _, dup := s.devs[d.Name]; dup {
		s.mu.Unlock()
		httpErr(w, 409, "device %s already enrolled", d.Name)
		return
	}
	s.devs[d.Name] = v
	s.mu.Unlock()
	v.mu.Lock()
	v.saveLocked()
	v.mu.Unlock()
	go s.deviceLoop(v)
	s.log.Printf("enrolled %s (%s, %s)", d.Name, d.Kind, req.Facts.DTModel)
	writeJSON(w, api.EnrollResponse{DeviceID: d.ID, Name: d.Name, Secret: secret, AgentEnrollment: s.agentEnrollment(d.Enrollment)})
}

// enrollment is what token t makes a Mac. A Volunteer's Mac enrolled again
// before its first check-in keeps the topic it was given (old).
func enrollment(t *api.EnrollTokenRequest, old api.Enrollment) api.Enrollment {
	if t.Volunteer == "" {
		return api.Enrollment{Kind: api.KindLabOwned}
	}
	e := api.Enrollment{Kind: api.KindVolunteer, Volunteer: t.Volunteer, Contact: t.Contact, NtfyTopic: old.NtfyTopic}
	if e.NtfyTopic == "" {
		e.NtfyTopic = newNtfyTopic()
	}
	return e
}

// newNtfyTopic is an unguessable ntfy topic for one Volunteer Mac: about 106
// random bits, in digits only so that no topic spells a word or a name.
func newNtfyTopic() string {
	n, _ := rand.Int(rand.Reader, new(big.Int).Exp(big.NewInt(10), big.NewInt(32), nil))
	return fmt.Sprintf("maclab-%032d", n)
}

// agentEnrollment is e as the Mac is told it, with its topic's URL on the
// lab's ntfy server (the one --ntfy posts to).
func (s *Server) agentEnrollment(e api.Enrollment) api.AgentEnrollment {
	ae := api.AgentEnrollment{Enrollment: e}
	if u, err := url.Parse(s.cfg.NtfyURL); err == nil && e.NtfyTopic != "" && u.Host != "" {
		ae.NtfyURL = (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/" + e.NtfyTopic}).String()
	}
	return ae
}

// hAgentEnrollment tells a Mac what it is enrolled as, for `lab-agent status`.
func (s *Server) hAgentEnrollment(w http.ResponseWriter, r *http.Request) {
	v := agentDev(r)
	v.mu.Lock()
	e := v.d.Enrollment
	v.mu.Unlock()
	writeJSON(w, s.agentEnrollment(e))
}

func (s *Server) hCheckin(w http.ResponseWriter, r *http.Request) {
	var ci api.Checkin
	if err := json.NewDecoder(r.Body).Decode(&ci); err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	v := agentDev(r)
	v.heartbeat(ci)
	// Long poll: hold the check-in until there is a command for this boot.
	hold, _ := strconv.Atoi(r.URL.Query().Get("wait_ms"))
	wait := min(time.Duration(hold)*time.Millisecond, s.cfg.Interval)
	if wait > 0 {
		v.waitFor(r.Context(), wait, func() bool { return len(v.pending) > 0 || v.d.BootID != ci.BootID })
	}
	writeJSON(w, v.deliver(ci.BootID))
}

func (s *Server) hReport(w http.ResponseWriter, r *http.Request) {
	var rep api.Report
	if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	agentDev(r).report(rep)
	w.Write([]byte("ok"))
}

var reSHA = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (s *Server) artifactPath(sha string) string {
	return filepath.Join(s.cfg.DataDir, "artifacts", sha)
}

func (s *Server) hAgentArtifact(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if !reSHA.MatchString(sha) {
		httpErr(w, 400, "bad sha256")
		return
	}
	http.ServeFile(w, r, s.artifactPath(sha))
}

func (s *Server) hPutArtifact(w http.ResponseWriter, r *http.Request) {
	sha, n, err := s.storeArtifact(r.Body)
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	writeJSON(w, map[string]any{"sha256": sha, "size": n})
}

// storeArtifact saves content under its sha256.
func (s *Server) storeArtifact(r io.Reader) (string, int64, error) {
	tmp, err := os.CreateTemp(filepath.Join(s.cfg.DataDir, "artifacts"), ".upload-*")
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	tmp.Close()
	if err != nil {
		return "", 0, err
	}
	sha := hex.EncodeToString(h.Sum(nil))
	now := s.now()
	os.Chtimes(tmp.Name(), now, now)
	return sha, n, os.Rename(tmp.Name(), s.artifactPath(sha))
}

// cleanName keeps uploaded file names inside the job directory.
func cleanName(name string) (string, bool) {
	c := filepath.ToSlash(filepath.Clean("/" + name))[1:]
	if c == "" || strings.HasPrefix(c, "..") {
		return "", false
	}
	return c, true
}

func (s *Server) hUpload(w http.ResponseWriter, r *http.Request) {
	job := r.URL.Query().Get("job")
	name, ok := cleanName(r.URL.Query().Get("name"))
	if !ok || strings.ContainsAny(job, "/.") || job == "" {
		httpErr(w, 400, "bad job or name")
		return
	}
	j, err := s.store.job(job)
	if err != nil || j.Spec.Device != agentDev(r).d.Name {
		httpErr(w, 404, "no such job for this device")
		return
	}
	p := filepath.Join(s.jobDir(job), name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	f, err := os.Create(p)
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	_, err = io.Copy(f, r.Body)
	f.Close()
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	w.Write([]byte("ok"))
}

// DeviceView is a device plus what labd derives about it.
type DeviceView struct {
	api.Device
	Tier     int       `json:"tier"`
	Alive    bool      `json:"alive"`
	Queued   int       `json:"queued"`
	ScreenAt time.Time `json:"screen_at,omitempty"`
}

func (s *Server) view(v *dev) DeviceView {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.viewLocked()
}

func (s *Server) hDevices(w http.ResponseWriter, r *http.Request) {
	out := []DeviceView{}
	for _, v := range s.allDevs() {
		out = append(out, s.view(v))
	}
	writeJSON(w, out)
}

func (s *Server) devOr404(w http.ResponseWriter, r *http.Request) *dev {
	v := s.dev(r.PathValue("name"))
	if v == nil {
		httpErr(w, 404, "no device %q", r.PathValue("name"))
	}
	return v
}

func (s *Server) hDevice(w http.ResponseWriter, r *http.Request) {
	if v := s.devOr404(w, r); v != nil {
		writeJSON(w, struct {
			DeviceView
			Events []api.KernelEvent `json:"recent_events"`
		}{s.view(v), v.eventsSince(s.now().Add(-24 * time.Hour))})
	}
}

func (s *Server) hPatchDevice(w http.ResponseWriter, r *http.Request) {
	v := s.devOr404(w, r)
	if v == nil {
		return
	}
	var p api.DevicePatch
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	v.mu.Lock()
	if p.Label != nil {
		v.d.Label = *p.Label
	}
	if p.Model != nil {
		v.d.Model = *p.Model
	}
	oobChanged := p.OOB != nil || p.NoOOB
	if p.OOB != nil {
		v.d.OOB = p.OOB
	}
	if p.NoOOB {
		v.d.OOB = nil
	}
	v.saveLocked()
	v.mu.Unlock()
	if oobChanged {
		s.attachOOB(v)
	}
	writeJSON(w, s.view(v))
}

func (s *Server) hLease(w http.ResponseWriter, r *http.Request) {
	v := s.devOr404(w, r)
	if v == nil {
		return
	}
	var req api.LeaseRequest
	json.NewDecoder(r.Body).Decode(&req)
	if req.Holder == "" {
		httpErr(w, 400, "holder required")
		return
	}
	if req.TTLSec <= 0 {
		req.TTLSec = 3600
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if l := v.d.Lease; l != nil && s.now().Before(l.Expires) && l.Holder != req.Holder {
		httpErr(w, 409, "%s is leased by %s until %s", v.d.Name, l.Holder, l.Expires.Format(time.TimeOnly))
		return
	}
	v.d.Lease = &api.Lease{Holder: req.Holder, Expires: s.now().Add(time.Duration(req.TTLSec) * time.Second)}
	v.saveLocked()
	writeJSON(w, v.d.Lease)
}

func (s *Server) hRelease(w http.ResponseWriter, r *http.Request) {
	v := s.devOr404(w, r)
	if v == nil {
		return
	}
	holder := r.URL.Query().Get("holder")
	v.mu.Lock()
	defer v.mu.Unlock()
	if l := v.d.Lease; l != nil && holder != "" && l.Holder != holder && s.now().Before(l.Expires) {
		httpErr(w, 409, "leased by %s, not %s", l.Holder, holder)
		return
	}
	v.d.Lease = nil
	v.saveLocked()
	w.Write([]byte("ok"))
}

// hReset runs the recovery ladder by hand, e.g. for an idle Mac that is wedged.
func (s *Server) hReset(w http.ResponseWriter, r *http.Request) {
	v := s.devOr404(w, r)
	if v == nil {
		return
	}
	by := r.URL.Query().Get("by")
	v.mu.Lock()
	busy := v.d.State == api.StateBusy || v.d.State == api.StateRecovering
	lease := v.d.Lease
	v.mu.Unlock()
	if busy {
		httpErr(w, 409, "%s is busy; cancel its job instead", v.d.Name)
		return
	}
	// A reset reboots the Mac, so a lease fences it like a job.
	if lease != nil && s.now().Before(lease.Expires) && lease.Holder != by {
		httpErr(w, 409, "%s is leased by %s until %s; only the lease holder can reset it", v.d.Name, lease.Holder, lease.Expires.Format(time.DateTime))
		return
	}
	go func() {
		steps, ok := v.ladder(s.ctx, "reset requested by "+by, s.cfg.BootTimeout, true)
		s.log.Printf("%s: manual reset: ok=%v steps=%v", v.d.Name, ok, steps)
	}()
	w.Write([]byte("recovery started; watch `lab status " + v.d.Name + "`\n"))
}

func (s *Server) hSerial(w http.ResponseWriter, r *http.Request) {
	v := s.devOr404(w, r)
	if v == nil {
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("lines"))
	if n <= 0 {
		n = 100
	}
	b, _ := os.ReadFile(s.serialPath(v.d.Name))
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	w.Header().Set("Content-Type", "text/plain")
	io.WriteString(w, strings.Join(lines, "\n")+"\n")
}

func (s *Server) hOOBStatus(w http.ResponseWriter, r *http.Request) {
	v := s.devOr404(w, r)
	if v == nil {
		return
	}
	v.mu.Lock()
	ctl, up, last := v.oob, v.serialUp, v.serialLast
	v.mu.Unlock()
	if ctl == nil {
		httpErr(w, 404, "%s has no out-of-band controller", v.d.Name)
		return
	}
	st, err := ctl.Status(r.Context())
	out := map[string]any{"controller": ctl.Name(), "serial_connected": up, "serial_last": last, "status": st}
	if err != nil {
		out["error"] = err.Error()
	}
	writeJSON(w, out)
}

func (s *Server) hSubmit(w http.ResponseWriter, r *http.Request) {
	var spec api.JobSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	j, code, err := s.submit(maintainerName(r), spec)
	if err != nil {
		httpErr(w, code, "%v", err)
		return
	}
	writeJSON(w, j)
}

// Submit validates and queues a job that labd queues itself, for no Maintainer.
func (s *Server) Submit(spec api.JobSpec) (*api.Job, int, error) { return s.submit("", spec) }

// submit validates and queues a job for the Maintainer who asked.
func (s *Server) submit(maintainer string, spec api.JobSpec) (*api.Job, int, error) {
	v := s.dev(spec.Device)
	if v == nil {
		return nil, 404, fmt.Errorf("no device %q", spec.Device)
	}
	if spec.Config != "" {
		if spec.Source == "" {
			return nil, 400, errors.New("--config applies to a kernel built from a source URL")
		}
		if !reSHA.MatchString(spec.Config) {
			return nil, 400, errors.New("config must be an artifact sha256 (upload it first)")
		}
		if _, err := os.Stat(s.artifactPath(spec.Config)); err != nil {
			return nil, 400, fmt.Errorf("config artifact %s not uploaded", spec.Config)
		}
	}
	if spec.Source != "" {
		if spec.Kernel != "" {
			return nil, 400, errors.New("give a kernel artifact or a source to build, not both")
		}
		if spec.Baseline || spec.Crash != "" {
			return nil, 400, errors.New("baseline and crash-test jobs run the known-good kernel; drop the source")
		}
		if _, err := parseSource(spec.Source); err != nil {
			return nil, 400, err
		}
	}
	if spec.Kernel != "" {
		if !reSHA.MatchString(spec.Kernel) {
			return nil, 400, errors.New("kernel must be an artifact sha256 (upload it first)")
		}
		if _, err := os.Stat(s.artifactPath(spec.Kernel)); err != nil {
			return nil, 400, fmt.Errorf("artifact %s not uploaded", spec.Kernel)
		}
	}
	for _, t := range spec.Tests {
		if t.Script != "" {
			if _, err := os.Stat(s.artifactPath(t.Script)); err != nil {
				return nil, 400, fmt.Errorf("test %s: script artifact %s not uploaded", t.Name, t.Script)
			}
		}
		if t.Name == "" || strings.ContainsAny(t.Name, "/ ") {
			return nil, 400, fmt.Errorf("test name %q must be non-empty with no slashes or spaces", t.Name)
		}
	}
	if spec.Crash != "" && spec.Crash != "panic" {
		return nil, 400, fmt.Errorf("unknown crash mode %q", spec.Crash)
	}
	d := s.view(v)
	if spec.CmdlineBase != "" || len(spec.CmdlineStrip) > 0 {
		if spec.Baseline || spec.Crash != "" {
			return nil, 400, errors.New("baseline and crash-test jobs boot the known-good cmdline; drop --cmdline-base/--cmdline-strip")
		}
		if !versionAtLeast(d.Facts.AgentVersion, "0.5.0") {
			return nil, 409, fmt.Errorf("%s runs lab-agent %s, which ignores --cmdline-base/--cmdline-strip; update it to 0.5.0 or later", d.Name, orStr(d.Facts.AgentVersion, "(unknown)"))
		}
		for _, g := range spec.CmdlineStrip {
			if _, err := path.Match(g, ""); err != nil || strings.TrimSpace(g) == "" {
				return nil, 400, fmt.Errorf("--cmdline-strip pattern %q is not a valid glob", g)
			}
		}
	}
	if spec.Crash == "" {
		has := false
		for i, t := range spec.Tests {
			if t.Builtin == omtTest {
				has = true
				spec.Tests[i].GUI = true
				if t.TimeoutSec == 0 {
					spec.Tests[i].TimeoutSec = omtSpec().TimeoutSec
				}
			}
		}
		if !has && s.omtWanted(v.snapshot(), spec) {
			spec.Tests = append(spec.Tests, omtSpec())
		}
	}
	if d.KnownGood == "" && !spec.Baseline {
		return nil, 409, fmt.Errorf("%s has not passed a baseline yet: run `lab baseline %s` first", d.Name, d.Name)
	}
	if spec.InstalledManifest != "" {
		if code, err := s.validateInstalled(spec, d.Device); err != nil {
			return nil, code, err
		}
		if p := installedPreflight(d.Facts.Problems); !d.Facts.PreflightOK && len(p) > 0 {
			return nil, 409, fmt.Errorf("%s fails preflight: %s", d.Name, strings.Join(p, "; "))
		}
	} else if !d.Facts.PreflightOK {
		return nil, 409, fmt.Errorf("%s fails preflight: %s", d.Name, strings.Join(d.Facts.Problems, "; "))
	}
	if l := d.Lease; l != nil && l.Holder != spec.Holder {
		return nil, 409, fmt.Errorf("%s is leased by %s until %s", d.Name, l.Holder, l.Expires.Format(time.TimeOnly))
	}
	now := s.now()
	j := &api.Job{ID: "j" + now.Format("0102-150405") + "-" + randHex(2), Spec: spec, Maintainer: maintainer, State: api.JobQueued, Created: now, Updated: now,
		Events: []api.JobEvent{{Time: now, Msg: "queued"}}}
	if err := s.store.saveJob(j); err != nil {
		return nil, 500, err
	}
	v.queue <- j.ID
	return j, 200, nil
}

func (s *Server) hJobs(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if n <= 0 {
		n = 20
	}
	jobs, err := s.store.jobs(r.URL.Query().Get("device"), n, false)
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	if jobs == nil {
		jobs = []*api.Job{}
	}
	writeJSON(w, jobs)
}

func (s *Server) hJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.store.job(r.PathValue("id"))
	if err != nil {
		httpErr(w, 404, "no job %s", r.PathValue("id"))
		return
	}
	writeJSON(w, j)
}

// hWait long-polls until the job is done or the timeout passes.
func (s *Server) hWait(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sec, _ := strconv.Atoi(r.URL.Query().Get("timeout"))
	if sec <= 0 || sec > 3600 {
		sec = 60
	}
	deadline := s.now().Add(time.Duration(sec) * time.Second)
	for {
		j, err := s.store.job(id)
		if err != nil {
			httpErr(w, 404, "no job %s", id)
			return
		}
		if j.State == api.JobDone || s.now().After(deadline) {
			writeJSON(w, j)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.clock().After(500 * time.Millisecond):
		}
	}
}

// hPublish uploads a finished job's omarchy-m-test report to omarchy-m-testing.org.
func (s *Server) hPublish(w http.ResponseWriter, r *http.Request) {
	j, err := s.store.job(r.PathValue("id"))
	if err != nil {
		httpErr(w, 404, "no job %s", r.PathValue("id"))
		return
	}
	if j.State != api.JobDone {
		httpErr(w, 409, "job %s is still %s", j.ID, j.State)
		return
	}
	url, err := s.omtPublish(r.Context(), j)
	if err != nil {
		httpErr(w, 409, "not published: %v", err)
		return
	}
	writeJSON(w, map[string]string{"report_url": url})
}

func (s *Server) hCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	j, err := s.store.job(id)
	if err != nil {
		httpErr(w, 404, "no job %s", id)
		return
	}
	s.mu.Lock()
	cancel := s.cancels[id]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		w.Write([]byte("canceling; the Mac is being restored to its known-good kernel\n"))
		return
	}
	if j.State == api.JobQueued {
		s.updateJob(j, func(j *api.Job) {
			j.State, j.Outcome, j.Summary, j.Canceled = api.JobDone, api.OutcomeCanceled, "canceled before it started", true
		})
	}
	w.Write([]byte("canceled\n"))
}

func (s *Server) hJobFile(w http.ResponseWriter, r *http.Request) {
	name, ok := cleanName(r.PathValue("name"))
	if !ok || strings.ContainsAny(r.PathValue("id"), "/.") {
		httpErr(w, 400, "bad name")
		return
	}
	http.ServeFile(w, r, filepath.Join(s.jobDir(r.PathValue("id")), name))
}
