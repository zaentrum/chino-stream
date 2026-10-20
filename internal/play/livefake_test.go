package play

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/live-worker/api"
	"github.com/zaentrum/live-worker/cmaf"
)

// fakeWorker is a live worker for the tests: an httptest server speaking the
// worker's contract (its api package) — a segment answered with a real CMAF
// init and fragment written by the worker's own cmaf package, an init alone,
// a subtitle stream as WebVTT, /readyz — that records every call.
type fakeWorker struct {
	t   *testing.T
	srv *httptest.Server

	mu    sync.Mutex
	calls []workerCall
	// answer, when set, may answer a call itself: a status and an error
	// body, 0 to answer it as the worker would.
	answer func(c workerCall) (int, api.ErrorBody)
	// pps is the video rungs' PPS: another one is another codec hash.
	pps   []byte
	vtt   string
	ready int // /readyz's status
	// hangUp closes that many connections without an answer.
	hangUp int
}

// workerCall is one call the fake got, decoded.
type workerCall struct {
	path string
	auth string
	req  api.Request
	sub  api.SubtitleRequest
	at   time.Time
}

// An SPS and a PPS of libx264 (High, level 4.0): the fake's video inits are
// real avcC headers.
var (
	fakeSPS, _ = hex.DecodeString("67640028acd941419f9f011000000300100000030300f1831960")
	fakePPS, _ = hex.DecodeString("68ebe3cb22c0")
	// fakePPS2 is another PPS: a rung whose parameter sets changed.
	fakePPS2, _ = hex.DecodeString("68ebe3cb22c1")
	// fakeASC is AAC-LC, 48 kHz, stereo.
	fakeASC = []byte{0x11, 0x90}
)

func newFakeWorker(t *testing.T) *fakeWorker {
	t.Helper()
	f := &fakeWorker{t: t, pps: fakePPS, vtt: "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nHello.\n\n", ready: http.StatusOK}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

// live is a LiveWorker of the fake in mode, with the token "tok".
func (f *fakeWorker) live(mode LiveWorkerMode) *LiveWorker {
	lw := NewLiveWorker(f.srv.URL, "tok", mode)
	lw.readyTTL = 0
	return lw
}

// got is the calls so far to path ("" every one).
func (f *fakeWorker) got(path string) []workerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []workerCall
	for _, c := range f.calls {
		if path == "" || c.path == path {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeWorker) set(fn func(f *fakeWorker)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeWorker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/readyz" {
		f.mu.Lock()
		ready := f.ready
		f.mu.Unlock()
		w.WriteHeader(ready)
		return
	}
	c := workerCall{path: r.URL.Path, auth: r.Header.Get("Authorization"), at: time.Now()}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var err error
	switch r.URL.Path {
	case "/v1/segment":
		if err = dec.Decode(&c.req); err == nil {
			err = c.req.Normalize(api.KindSegment)
		}
	case "/v1/init":
		if err = dec.Decode(&c.req); err == nil {
			err = c.req.Normalize(api.KindInit)
		}
	case "/v1/subtitle":
		if err = dec.Decode(&c.sub); err == nil {
			err = c.sub.Normalize()
		}
	default:
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	hangUp := f.hangUp > 0
	if hangUp {
		f.hangUp--
	}
	answer, pps, vtt := f.answer, f.pps, f.vtt
	f.mu.Unlock()
	if hangUp {
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
		return
	}
	if c.auth != "Bearer tok" {
		fakeFail(w, http.StatusUnauthorized, api.ErrorBody{Error: "a bearer token is required", Class: api.ClassUnauthorized})
		return
	}
	if err != nil {
		fakeFail(w, http.StatusBadRequest, api.ErrorBody{Error: err.Error(), Class: api.ClassBad})
		return
	}
	src := c.req.Source.Key()
	if c.path == "/v1/subtitle" {
		src = c.sub.Source.Path
	}
	if _, err := os.Stat(src); err != nil {
		fakeFail(w, http.StatusUnprocessableEntity, api.ErrorBody{Error: err.Error(), Class: api.ClassSource, Scope: api.ScopeSource})
		return
	}
	if answer != nil {
		if status, body := answer(c); status != 0 {
			fakeFail(w, status, body)
			return
		}
	}
	if c.path == "/v1/subtitle" {
		w.Header().Set("Content-Type", "text/vtt")
		_, _ = w.Write([]byte(vtt))
		return
	}
	init, frag, res, err := fakeOutput(c, pps)
	if err != nil {
		fakeFail(w, http.StatusBadGateway, api.ErrorBody{Error: err.Error(), Class: api.ClassInternal})
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set(api.HeaderCodec, res.CodecHash)
	w.Header().Set(api.HeaderResult, api.EncodeResult(res))
	w.Header().Set(api.HeaderAttempts, "1")
	if c.path == "/v1/segment" {
		w.Header().Set(api.HeaderInitLength, strconv.Itoa(len(init)))
	}
	_, _ = w.Write(append(init, frag...))
}

func fakeFail(w http.ResponseWriter, status int, body api.ErrorBody) {
	if body.RetryAfterMs > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, (body.RetryAfterMs+999)/1000)))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// fakeOutput is what the worker makes of a call: a video rung at 24 fps on a
// 90 kHz timeline, or AAC frames on the 48 kHz grid; the fragment nil for an
// init.
func fakeOutput(c workerCall, pps []byte) (init, frag []byte, res api.Result, err error) {
	var start, end float64
	var last bool
	if s := c.req.Segment; s != nil {
		var ok bool
		if start, end, last, ok, err = s.Resolve(0); err != nil || !ok {
			return nil, nil, res, fmt.Errorf("segment %+v: %v", s, err)
		}
	}
	res = api.Result{Job: "fake", Start: start, End: end, Last: last}
	if c.req.Segment != nil {
		res.Index = c.req.Segment.Index
	}
	if c.req.Track == api.TrackAudio {
		res.Path, res.Timescale = "aac", 48000
		if init, res.CodecHash, err = cmaf.AudioInit(48000, 2, fakeASC, nil); err != nil || c.req.Segment == nil {
			res.InitLength = len(init)
			return init, nil, res, err
		}
		n0, n1 := int64(math.Round(start*48000/1024)), int64(math.Round(end*48000/1024))
		var samples []cmaf.Sample
		for n := n0; n < n1; n++ {
			samples = append(samples, cmaf.Sample{PTS: n * 1024, Dur: 1024, Key: true, Data: []byte{0x21, 0x10, byte(n)}})
		}
		frag, info, err := cmaf.AudioFragment(uint32(res.Index+1), samples)
		res.Frames, res.InitLength = info.Frames, len(init)
		return init, frag, res, err
	}
	res.Path, res.Timescale = "cpu", 90000
	if c.req.Video != nil {
		res.Width, res.Height = c.req.Video.Width, c.req.Video.Height
	}
	config := append(append([]byte{0, 0, 0, 1}, fakeSPS...), append([]byte{0, 0, 0, 1}, pps...)...)
	if init, res.CodecHash, err = cmaf.VideoInit(90000, config, nil); err != nil || c.req.Segment == nil {
		res.InitLength = len(init)
		return init, nil, res, err
	}
	first := int64(math.Round(start * 90000))
	n := max(1, int(math.Round((end-start)*24)))
	var samples []cmaf.Sample
	for i := 0; i < n; i++ {
		nal := []byte{0, 0, 0, 2, 0x41, byte(i)}
		if i == 0 {
			nal = []byte{0, 0, 0, 2, 0x65, 0x88}
		}
		samples = append(samples, cmaf.Sample{PTS: first + int64(i)*3750, Key: i == 0, Data: nal})
	}
	frag, info, err := cmaf.VideoFragment(uint32(res.Index+1), samples, max(int64(math.Round(end*90000)), first+int64(n)*3750))
	res.Frames, res.InitLength, res.FirstPTS, res.EndPTS = info.Frames, len(init), info.EarliestPTS, info.EndPTS
	return init, frag, res, err
}

// settle waits until the live worker of h has no job and no shadow check
// under way.
func settle(t *testing.T, lw *LiveWorker) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		lw.flights.mu.Lock()
		n := len(lw.flights.m)
		lw.flights.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d live jobs still under way", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	lw.shadow.wg.Wait()
}

// inPlaceWarms runs the cache warms in place for the test.
func inPlaceWarms(t *testing.T) {
	t.Helper()
	old := goWarm
	goWarm = func(f func()) { f() }
	t.Cleanup(func() { goWarm = old })
}
