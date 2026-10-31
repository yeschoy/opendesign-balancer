package config

import (
	"strings"
	"testing"
)

func TestRiverAccountProvider(t *testing.T) {
	c := Default()
	c.Accounts = []Account{
		{ID: "a1", RuntimeKey: "rv_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG", Group: "river"},
		{ID: "a2", RuntimeKey: "sk-amr-abcdef", Provider: ProviderRiver},
	}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if c.Accounts[0].Provider != ProviderRiver || c.Accounts[1].Provider != ProviderOpenDesign {
		t.Fatalf("providers: %q %q", c.Accounts[0].Provider, c.Accounts[1].Provider)
	}
	if got := c.RuntimeBase(c.Accounts[0]); got != "https://api.river.ai/v1" {
		t.Fatalf("river base %q", got)
	}
	if got := c.RuntimeBase(c.Accounts[1]); got != c.Upstream.RuntimeBase {
		t.Fatalf("opendesign base %q", got)
	}
	c.Accounts[0].ControlKey = "vela_ctrl_x"
	if err := c.Normalize(); err == nil {
		t.Fatal("river account with control key must be rejected")
	}
}

func TestProvidersGetSeparateGroups(t *testing.T) {
	c := Default()
	c.Accounts = []Account{
		{ID: "a1", RuntimeKey: "sk-amr-aaaaaaaa"},
		{ID: "a2", RuntimeKey: "rv_bbbbbbbbbbbbbbbbbbbbbbbb"},
	}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if c.Accounts[0].Group != DefaultGroup || c.Accounts[1].Group != RiverGroup {
		t.Fatalf("groups: %q %q", c.Accounts[0].Group, c.Accounts[1].Group)
	}
	c.Accounts[1].Group = DefaultGroup // mixing providers in one group is refused
	if err := c.Normalize(); err == nil {
		t.Fatal("mixed-provider group must be rejected")
	}
}

// An Ollama API key carries no prefix, so the account names its provider; a
// key that does carry one always wins over it.
func TestOllamaAccountProvider(t *testing.T) {
	c := Default()
	c.Accounts = []Account{
		{ID: "o1", RuntimeKey: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", Provider: ProviderOllama},
		{ID: "o2", RuntimeKey: "oll-abcdefghijklmnop", Group: OllamaGroup},
		{ID: "o3", RuntimeKey: "ollama", Provider: ProviderOllama, Group: "local"},
	}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	for _, a := range c.Accounts {
		if a.Provider != ProviderOllama {
			t.Fatalf("%s: provider %q", a.ID, a.Provider)
		}
		if got := c.RuntimeBase(a); got != "https://ollama.com/v1" {
			t.Fatalf("%s: base %q", a.ID, got)
		}
	}
	if c.Accounts[0].Group != OllamaGroup {
		t.Fatalf("default group %q", c.Accounts[0].Group)
	}
	// a key with another pool's prefix overrides the declared provider (and
	// the account then has to leave the Ollama group)
	c.Accounts[0].RuntimeKey, c.Accounts[0].Group = "sk-amr-swapped", DefaultGroup
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if c.Accounts[0].Provider != ProviderOpenDesign {
		t.Fatalf("swapped key kept provider %q", c.Accounts[0].Provider)
	}
	// ... and an OpenDesign key may not stay in the Ollama group
	c.Accounts[0].Group = OllamaGroup
	if err := c.Normalize(); err == nil {
		t.Fatal("opendesign account in the ollama group must be rejected")
	}
}

// An Ollama account has no control key: its API key reads its own balance.
func TestOllamaRejectsControlKey(t *testing.T) {
	c := Default()
	c.Accounts = []Account{{ID: "o1", RuntimeKey: "opaque-key-value", Provider: ProviderOllama, ControlKey: "vela_ctrl_x"}}
	if err := c.Normalize(); err == nil {
		t.Fatal("ollama account with control key must be rejected")
	}
}

// The Ollama pool gets its own group default and its own stock rules.
func TestOllamaPoolSeparate(t *testing.T) {
	c := Default()
	c.Accounts = []Account{
		{ID: "o1", RuntimeKey: "opaque-key-value", Provider: ProviderOllama},
		{ID: "d1", RuntimeKey: "sk-amr-aaaaaaaa"},
	}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if c.Accounts[0].Group != OllamaGroup || c.Accounts[1].Group != DefaultGroup {
		t.Fatalf("groups %q %q", c.Accounts[0].Group, c.Accounts[1].Group)
	}
	c.Accounts[0].Group = DefaultGroup // mixing pools in one group is refused
	if err := c.Normalize(); err == nil {
		t.Fatal("mixed-provider group must be rejected")
	}
	c.OllamaErrorRules = nil
	c.Accounts = nil
	if err := c.Normalize(); err != nil || len(c.OllamaErrorRules) == 0 {
		t.Fatalf("ollama rules not defaulted: %v", err)
	}
	od, _ := c.RulesFor(ProviderOpenDesign)
	ol, _ := c.RulesFor(ProviderOllama)
	if len(ol) == 0 || len(od) == 0 || ol[0].ID == od[0].ID {
		t.Fatal("ollama must judge failures by its own rules")
	}
}

// The balance lives on the host of the inference base, without /v1.
func TestOllamaAPIRoot(t *testing.T) {
	for in, want := range map[string]string{
		"https://ollama.com/v1":        "https://ollama.com",
		"https://ollama.com/v1/":       "https://ollama.com",
		"http://127.0.0.1:11434/v1":    "http://127.0.0.1:11434",
		"http://127.0.0.1:11434":       "http://127.0.0.1:11434",
		"https://proxy.example/ollama": "https://proxy.example/ollama",
	} {
		if got := OllamaAPIRoot(in); got != want {
			t.Fatalf("%s: got %q want %q", in, got, want)
		}
	}
}

// The three things that must stay per pool: the group (号池), the downstream
// key, and the error/retry rules. Nothing may fall back to another pool's.
func TestPoolsAreFullySeparate(t *testing.T) {
	c := Default()
	c.Accounts = []Account{
		{ID: "d1", RuntimeKey: "sk-amr-aaaaaaaa"},
		{ID: "r1", RuntimeKey: "rv_bbbbbbbbbbbbbbbbbbbb"},
		{ID: "o1", RuntimeKey: "opaque-ollama-key", Provider: ProviderOllama},
	}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	// 1. one group per pool, and no group is shared
	seen := map[string]string{}
	for _, a := range c.Accounts {
		if p, ok := seen[a.Group]; ok && p != a.Provider {
			t.Fatalf("group %q shared by %s and %s", a.Group, p, a.Provider)
		}
		seen[a.Group] = a.Provider
	}
	if seen["default"] != ProviderOpenDesign || seen["river"] != ProviderRiver || seen["ollama"] != ProviderOllama {
		t.Fatalf("groups = %v", seen)
	}

	// 2. keys belong to their pool, including a group that holds no account
	c.ClientKeys = []ClientKey{
		{ID: "k1", Group: "default", KeyHash: HashKey("a")},
		{ID: "k2", Group: "river", KeyHash: HashKey("b")},
		{ID: "k3", Group: "ollama", KeyHash: HashKey("c")},
		{ID: "k4", Group: "ollama2", Provider: ProviderOllama, KeyHash: HashKey("d")},
	}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"k1": ProviderOpenDesign, "k2": ProviderRiver, "k3": ProviderOllama, "k4": ProviderOllama}
	for _, k := range c.ClientKeys {
		if k.Provider != want[k.ID] {
			t.Fatalf("%s: provider %q want %q", k.ID, k.Provider, want[k.ID])
		}
		if got := c.GroupPool(k.Group); got != want[k.ID] {
			t.Fatalf("%s: group %q resolves to pool %q", k.ID, k.Group, got)
		}
	}
	// a key may not sit in another pool's group
	c.ClientKeys = append(c.ClientKeys, ClientKey{ID: "k5", Group: "ollama", KeyHash: HashKey("e"), Provider: ProviderRiver})
	if err := c.Normalize(); err == nil {
		t.Fatal("a river key in the ollama group must be rejected")
	}
	c.ClientKeys = c.ClientKeys[:4]

	// 3. each pool reads its own rules, and each pool's defaults are its own
	for _, p := range Providers {
		errR, retR := c.RulesFor(p)
		if len(errR) == 0 || len(retR) == 0 {
			t.Fatalf("%s: empty rules", p)
		}
	}
	defErr := map[string]string{}
	defRet := map[string]string{}
	for _, p := range Providers {
		e, r := DefaultErrorRulesFor(p), DefaultRetryRulesFor(p)
		defErr[p], defRet[p] = e[0].ID, r[0].ID
	}
	if defErr[ProviderOpenDesign] == defErr[ProviderOllama] || defErr[ProviderRiver] == defErr[ProviderOllama] ||
		defRet[ProviderOpenDesign] == defRet[ProviderOllama] || defRet[ProviderRiver] == defRet[ProviderOllama] {
		t.Fatalf("shared defaults: %v %v", defErr, defRet)
	}
	// editing one pool's rules leaves the others alone
	before, _ := c.RulesFor(ProviderOpenDesign)
	c.OllamaErrorRules = c.OllamaErrorRules[:1]
	after, _ := c.RulesFor(ProviderOpenDesign)
	if len(before) != len(after) || len(after) == len(c.OllamaErrorRules) {
		t.Fatal("pools must not share one rule slice")
	}
}

// A key can't be moved into another pool by editing a field: the group's pool
// and the key's own pool have to agree.
func TestKeyCannotCrossPools(t *testing.T) {
	c := Default()
	c.ClientKeys = []ClientKey{
		{ID: "ol", Group: "ollama", Provider: ProviderOllama, KeyHash: HashKey("a")},
		{ID: "rv", Group: "river", Provider: ProviderRiver, KeyHash: HashKey("b")},
	}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	// declaring another pool inside a pool's own group is refused
	for _, tc := range []struct{ group, provider string }{{"river", ProviderOllama}, {"default", ProviderOllama}, {"ollama", ProviderRiver}} {
		c.ClientKeys = append(c.ClientKeys, ClientKey{ID: "x", Group: tc.group, Provider: tc.provider, KeyHash: HashKey(tc.group + tc.provider)})
		if err := c.Normalize(); err == nil {
			t.Fatalf("%s/%s: cross-pool key accepted", tc.group, tc.provider)
		}
		c.ClientKeys = c.ClientKeys[:2]
	}
	// a custom group follows the key that names it
	c.ClientKeys = append(c.ClientKeys, ClientKey{ID: "c", Group: "team-a", Provider: ProviderOllama, KeyHash: HashKey("c")})
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if p := c.GroupPool("team-a"); p != ProviderOllama {
		t.Fatalf("team-a pool = %q", p)
	}
}

// A Modal account is identified by its proxy token's wk- prefix, and it is
// served from one host for every model: on Modal the request's model field is
// the Endpoint's hostname, not a separate base URL per model.
func TestModalAccountProvider(t *testing.T) {
	c := Default()
	c.Accounts = []Account{
		{ID: "m1", RuntimeKey: "wk-abc123.ws-secret456"},
		{ID: "m2", RuntimeKey: "wk-abc123.ws-secret456-def", Provider: ProviderOpenDesign},
	}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	// the key's prefix always wins over a stale declared provider
	if c.Accounts[0].Provider != ProviderModal || c.Accounts[1].Provider != ProviderModal {
		t.Fatalf("providers: %q %q", c.Accounts[0].Provider, c.Accounts[1].Provider)
	}
	if c.Accounts[0].Group != ModalGroup {
		t.Fatalf("group %q", c.Accounts[0].Group)
	}
	// the base ends in /v1 like the other pools': the proxy joins every path
	// to it, and Modal's inference host answers 404 without the prefix
	if got := c.RuntimeBase(c.Accounts[0]); got != "https://inference.us-west.modal.direct/v1" {
		t.Fatalf("modal base %q", got)
	}

	// no control key: Modal has no control plane to read with one
	c.Accounts[0].ControlKey = "vela_ctrl_x"
	if err := c.Normalize(); err == nil {
		t.Fatal("a modal account with a control key must be rejected")
	}
	c.Accounts[0].ControlKey = ""

	// the token must be a proxy token, not any string
	c.Accounts[0].RuntimeKey = "not-a-modal-token"
	if err := c.Normalize(); err == nil {
		t.Fatal("a non-wk- key declared modal must be rejected")
	}
}

func TestModalKeyRecognition(t *testing.T) {
	for key, want := range map[string]string{
		"wk-example0000000000000000": ProviderModal,
		"wk-abc.ws-def":              ProviderModal,
		"rv_abc":                     ProviderRiver,
		"sk-amr-abc":                 ProviderOpenDesign,
		"oll-abc":                    ProviderOllama,
		"opaque":                     "",
	} {
		if got := ProviderForKey(key); got != want {
			t.Fatalf("ProviderForKey(%q) = %q, want %q", key, got, want)
		}
	}
	// a key whose prefix names a provider always settles it
	if got := ResolveProvider(ProviderRiver, "wk-abc.ws-def"); got != ProviderModal {
		t.Fatalf("ResolveProvider = %q", got)
	}
	if got := ResolveProvider("", "wk-abc.ws-def"); got != ProviderModal {
		t.Fatalf("ResolveProvider = %q", got)
	}
	if !ValidRuntimeKey("wk-abc.ws-def") || ValidRuntimeKey("wk-a b") {
		t.Fatal("modal key validation")
	}
	if m := Mask("wk-abc123.ws-verysecretend"); strings.Contains(m, "verysecretend") || !strings.HasPrefix(m, "wk-") {
		t.Fatalf("Mask leaked the token: %q", m)
	}
}

// The Modal pool keeps its own rules and its own group, and a Modal key can't
// be filed under another pool's group.
func TestModalPoolSeparate(t *testing.T) {
	c := Default()
	c.Accounts = []Account{
		{ID: "d1", RuntimeKey: "sk-amr-aaaaaaaa"},
		{ID: "m1", RuntimeKey: "wk-abc.ws-def"},
	}
	c.ClientKeys = []ClientKey{
		{ID: "km", Group: "modal", KeyHash: HashKey("m")},
		{ID: "km2", Group: "modal-x", Provider: ProviderModal, KeyHash: HashKey("m2")},
	}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if c.Accounts[1].Group != ModalGroup {
		t.Fatalf("modal group %q", c.Accounts[1].Group)
	}
	for _, k := range c.ClientKeys {
		if k.Provider != ProviderModal {
			t.Fatalf("%s: provider %q", k.ID, k.Provider)
		}
		if got := c.GroupPool(k.Group); got != ProviderModal {
			t.Fatalf("%s: group pool %q", k.ID, got)
		}
	}
	// a modal key may not sit in another pool's group
	c.ClientKeys = append(c.ClientKeys, ClientKey{ID: "kx", Group: "ollama", KeyHash: HashKey("x"), Provider: ProviderModal})
	if err := c.Normalize(); err == nil {
		t.Fatal("a modal key in the ollama group must be accepted only for its own pool")
	}
	c.ClientKeys = c.ClientKeys[:2]

	// its own rules, distinct from every other pool's
	if len(c.ModalErrorRules) == 0 || len(c.ModalRetryRules) == 0 {
		t.Fatal("modal rules missing")
	}
	errR, retR := c.RulesFor(ProviderModal)
	if errR[0].ID != "modal-token-invalid" || retR[0].ID != "modal-bad-request" {
		t.Fatalf("modal rules = %s / %s", errR[0].ID, retR[0].ID)
	}
	seen := map[string]bool{}
	for _, p := range Providers {
		e, r := DefaultErrorRulesFor(p), DefaultRetryRulesFor(p)
		if seen[e[0].ID] || seen[r[0].ID] {
			t.Fatalf("pool %s shares a default rule id", p)
		}
		seen[e[0].ID], seen[r[0].ID] = true, true
	}
	// an account of another pool stays that pool's
	if _, ret := c.RulesFor(ProviderOpenDesign); ret[0].ID == "modal-bad-request" {
		t.Fatal("modal rules leaked into opendesign")
	}
}

// A config written before the Modal pool starts from the Modal defaults, the
// same way the River and Ollama pools did.
func TestModalRulesBackfilled(t *testing.T) {
	c := Default()
	c.ModalErrorRules, c.ModalRetryRules = nil, nil
	c.Upstream.ModalBase = ""
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if len(c.ModalErrorRules) == 0 || len(c.ModalRetryRules) == 0 {
		t.Fatal("modal rules not backfilled")
	}
	if c.Upstream.ModalBase == "" {
		t.Fatal("modal base not backfilled")
	}
}

// Modal's host answers 404 without the /v1 every path is joined to, so a base
// entered the way the dashboard prints it (bare host) is completed rather
// than left to fail every request with somebody else's 404.
func TestModalBaseGetsV1(t *testing.T) {
	for in, want := range map[string]string{
		"https://inference.us-west.modal.direct":    "https://inference.us-west.modal.direct/v1",
		"https://inference.us-west.modal.direct/":   "https://inference.us-west.modal.direct/v1",
		"https://inference.us-west.modal.direct/v1": "https://inference.us-west.modal.direct/v1",
		"https://inference.eu.modal.direct/v1/":     "https://inference.eu.modal.direct/v1",
		// a base with its own path is left alone
		"https://gateway.example.com/modal/v1": "https://gateway.example.com/modal/v1",
	} {
		c := Default()
		c.Upstream.ModalBase = in
		if err := c.Normalize(); err != nil {
			t.Fatal(err)
		}
		if got := c.Upstream.ModalBase; got != want {
			t.Fatalf("ModalBase %q → %q, want %q", in, got, want)
		}
		a := Account{ID: "m", Provider: ProviderModal, RuntimeKey: "wk-a.b"}
		if got := c.RuntimeBase(a); got != want {
			t.Fatalf("RuntimeBase %q, want %q", got, want)
		}
	}
}

// Modal's credential is two parts. A token that is only the Token ID is
// accepted into the pool (it is the only value the operator may have) but is
// flagged, because Modal answers a bare ID with the same 401 on every host
// and path — that reads like an outage unless it is named.
func TestModalTokenIncomplete(t *testing.T) {
	for key, want := range map[string]bool{
		"wk-example0000000000":      true,
		"wk-abc":                    true,
		"wk-example0000000000.ws-x": false,
		"wk-abc.ws-def":             false,
		"sk-amr-abc":                false,
		"":                          false,
	} {
		if got := ModalTokenIncomplete(key); got != want {
			t.Fatalf("ModalTokenIncomplete(%q) = %v, want %v", key, got, want)
		}
	}
	// still a valid key shape: the pool takes it, only the read fails
	if !ValidRuntimeKeyFor(ProviderModal, "wk-example0000000000") {
		t.Fatal("a bare token id must still be accepted as a runtime key")
	}
}
