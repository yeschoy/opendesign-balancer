package config

import "testing"

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
