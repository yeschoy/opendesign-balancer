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
