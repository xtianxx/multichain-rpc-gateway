package chain

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestParseChainID(t *testing.T) {
	cases := []struct {
		in   any
		want string
		err  bool
	}{
		{"8453", "8453", false},
		{json.Number("8453"), "8453", false},
		{"08453", "8453", false},
		{"0", "0", false},
		{"000", "0", false},
		{"1", "1", false},
		{"0x2105", "", true},
		{"-1", "", true},
		{"1.5", "", true},
		{json.Number("1.5"), "", true},
		{json.Number("1e3"), "", true},
		{"", "", true},
		{"abc", "", true},
		{8453, "", true}, // plain int not accepted
	}
	for _, tc := range cases {
		got, err := ParseChainID(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("ParseChainID(%v): expected error, got %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseChainID(%v): unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseChainID(%v): got %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestAdapterRegistry(t *testing.T) {
	for _, name := range []string{"ethereum", "base"} {
		if _, ok := GetAdapter(name); !ok {
			t.Errorf("adapter %q must be registered", name)
		}
	}
	if _, ok := GetAdapter("solana"); ok {
		t.Error("unregistered adapter must not be found")
	}
	if got, want := AdapterNames(), []string{"base", "ethereum"}; !reflect.DeepEqual(got, want) {
		t.Errorf("AdapterNames(): got %v want %v", got, want)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("duplicate RegisterAdapter must panic")
			}
		}()
		RegisterAdapter(EthereumAdapter{})
	}()
}

func TestNewChain(t *testing.T) {
	u := &Upstream{Name: "mainnet-a", URL: mustURL(t, "https://mainnet.example.com")}
	c, err := NewChain("08453", "base", []*Upstream{u})
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	if c.ChainID != "8453" {
		t.Errorf("chain id not canonicalized: got %q", c.ChainID)
	}
	if c.Adapter.Name() != "base" {
		t.Errorf("adapter: got %q", c.Adapter.Name())
	}
	if _, err := NewChain("1", "solana", []*Upstream{u}); err == nil {
		t.Error("unknown adapter must error")
	}
	if _, err := NewChain("0x1", "ethereum", []*Upstream{u}); err == nil {
		t.Error("invalid chain id must error")
	}
}

func TestEIP1898Normalization(t *testing.T) {
	eth := EthereumAdapter{}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"unwraps-blockNumber", `["0xabc", {"blockNumber":"0x1"}]`, `["0xabc","0x1"]`},
		{"latest-passes", `["0xabc", {"blockNumber":"latest"}]`, `["0xabc","latest"]`},
		{"earliest-passes", `["0xabc", {"blockNumber":"earliest"}]`, `["0xabc","earliest"]`},
		{"pending-passes", `["0xabc", {"blockNumber":"pending"}]`, `["0xabc","pending"]`},
		{"safe-passes", `["0xabc", {"blockNumber":"safe"}]`, `["0xabc","safe"]`},
		{"finalized-passes", `["0xabc", {"blockNumber":"finalized"}]`, `["0xabc","finalized"]`},
		{"blockHash-passes", `["0xabc", {"blockHash":"0xdead"}]`, `["0xabc", {"blockHash":"0xdead"}]`},
		{"multi-key-passes", `["0xabc", {"blockNumber":"0x1","blockHash":"0x2"}]`, `["0xabc", {"blockNumber":"0x1","blockHash":"0x2"}]`},
		{"plain-array-passes", `["0xabc","latest"]`, `["0xabc","latest"]`},
		{"empty-array-passes", `[]`, `[]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := eth.NormalizeParams(json.RawMessage(tc.in))
			if err != nil {
				t.Fatalf("NormalizeParams: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("got %s want %s", got, tc.want)
			}
		})
	}

	// Named params (object) pass through untouched.
	named := `{"to":"0xabc"}`
	got, err := eth.NormalizeParams(json.RawMessage(named))
	if err != nil || string(got) != named {
		t.Errorf("named params: got %s err %v", got, err)
	}
	// Nil params stay nil.
	got, err = eth.NormalizeParams(nil)
	if err != nil || got != nil {
		t.Errorf("nil params: got %s err %v", got, err)
	}
}

func TestShapeResponse(t *testing.T) {
	eth := EthereumAdapter{}
	base := BaseAdapter{}

	if got := eth.ShapeResponse(json.RawMessage(`{"x":1}`)); string(got) != `{"x":1}` {
		t.Errorf("ethereum identity: got %s", got)
	}
	if got := base.ShapeResponse(json.RawMessage(`{"x":1}`)); string(got) != `{"x":1}` {
		t.Errorf("base identity: got %s", got)
	}

	// Base-specific: OP-stack error shape in a result payload is canonicalized.
	in := `{"code":3,"message":"execution reverted: blah"}`
	got := base.ShapeResponse(json.RawMessage(in))
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("shape output not JSON: %v (%s)", err, got)
	}
	if m["code"] != float64(-32000) || m["message"] != "execution reverted" {
		t.Errorf("base canonicalization: got %s", got)
	}
	// Non-code-3 error shapes pass through.
	if got := base.ShapeResponse(json.RawMessage(`{"code":5,"message":"x"}`)); string(got) != `{"code":5,"message":"x"}` {
		t.Errorf("non-3 code must pass through: got %s", got)
	}
	// Ethereum does not canonicalize.
	if got := eth.ShapeResponse(json.RawMessage(in)); string(got) != in {
		t.Errorf("ethereum must not canonicalize: got %s", got)
	}
}

func TestUpstreamStateRaceSafe(t *testing.T) {
	u := &Upstream{Name: "a", URL: mustURL(t, "https://a.example.com"), Client: &http.Client{}}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				u.SetHealth(HealthState(j % 3))
				u.SetLatency(time.Duration(j) * time.Millisecond)
				_ = u.Health()
				_ = u.Latency()
			}
		}(i)
	}
	wg.Wait()
	h := u.Health()
	if h < HealthUnknown || h > HealthUnhealthy {
		t.Errorf("health out of range: %d", h)
	}
	if u.Latency() < 0 {
		t.Errorf("latency negative: %v", u.Latency())
	}
}

func TestHealthStateValues(t *testing.T) {
	if HealthUnknown != 0 || HealthHealthy != 1 || HealthUnhealthy != 2 {
		t.Error("HealthState values must be 0/1/2")
	}
}

// fakeBreaker is a controllable Breaker double for the chain tests.
type fakeBreaker struct {
	state string
	calls int
	err   error
}

func (b *fakeBreaker) Execute(fn func() ([]byte, error)) ([]byte, error) {
	b.calls++
	if b.state == "open" {
		return nil, b.err
	}
	return fn()
}

func (b *fakeBreaker) State() string { return b.state }

func TestUpstreamExecuteDelegatesToBreaker(t *testing.T) {
	u := &Upstream{Name: "a", URL: mustURL(t, "https://a.example.com")}

	// No breaker wired: fn runs directly.
	ran := false
	got, err := u.Execute(func() ([]byte, error) { ran = true; return []byte("x"), nil })
	if err != nil || string(got) != "x" || !ran {
		t.Fatalf("nil-breaker Execute: got %s err %v ran %v", got, err, ran)
	}
	if u.BreakerOpen() {
		t.Error("nil breaker must not report open")
	}

	// Breaker wired: fn runs through it; open state short-circuits.
	b := &fakeBreaker{state: "closed"}
	u.SetBreaker(b)
	if _, err := u.Execute(func() ([]byte, error) { return nil, nil }); err != nil || b.calls != 1 {
		t.Fatalf("wired Execute must delegate once: calls %d err %v", b.calls, err)
	}
	if u.Breaker() != b {
		t.Error("Breaker() must return the wired breaker")
	}

	b.state = "open"
	b.err = errors.New("circuit open")
	ran = false
	if _, err := u.Execute(func() ([]byte, error) { ran = true; return nil, nil }); err == nil {
		t.Error("open breaker must short-circuit with error")
	}
	if ran {
		t.Error("open breaker must not run fn")
	}
	if !u.BreakerOpen() {
		t.Error("open state must report BreakerOpen")
	}
	b.state = "half-open"
	if u.BreakerOpen() {
		t.Error("half-open must not report BreakerOpen")
	}
}

func TestUpstreamProbeStreak(t *testing.T) {
	u := &Upstream{Name: "a", URL: mustURL(t, "https://a.example.com")}
	if u.FailStreak() != 0 {
		t.Fatal("initial streak must be 0")
	}
	if got := u.RecordProbeFail(); got != 1 {
		t.Fatalf("first fail: got %d", got)
	}
	if got := u.RecordProbeFail(); got != 2 {
		t.Fatalf("second fail: got %d", got)
	}
	u.RecordProbeOK()
	if u.FailStreak() != 0 {
		t.Fatal("probe ok must reset streak")
	}
}

func TestUpstreamRecordLatencyEWMA(t *testing.T) {
	u := &Upstream{Name: "a", URL: mustURL(t, "https://a.example.com")}
	// First sample taken as-is.
	u.RecordLatency(100 * time.Millisecond)
	if u.Latency() != 100*time.Millisecond {
		t.Fatalf("first sample: got %v", u.Latency())
	}
	// alpha 0.25: next = prev + (d-prev)*0.25
	u.RecordLatency(200 * time.Millisecond)
	if got := u.Latency(); got != 125*time.Millisecond {
		t.Fatalf("ewma step: got %v want 125ms", got)
	}
	// Raw SetLatency accessor still works alongside.
	u.SetLatency(42 * time.Millisecond)
	if u.Latency() != 42*time.Millisecond {
		t.Fatalf("raw accessor: got %v", u.Latency())
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url %q: %v", raw, err)
	}
	return u
}
