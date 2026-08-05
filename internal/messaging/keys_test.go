package messaging

import "testing"

func TestKeyBuilders(t *testing.T) {
	if got := FlagKey(3, "search_v2"); got != "3.search_v2" {
		t.Errorf("FlagKey = %q", got)
	}
	if got := MicroKey(3, 42); got != "3.42" {
		t.Errorf("MicroKey = %q", got)
	}
	if got := LocalizationKey(3, 42, "pt-BR"); got != "3.42.pt-BR" {
		t.Errorf("LocalizationKey = %q", got)
	}
	if got := EnvironmentPrefix(3); got != "3.>" {
		t.Errorf("EnvironmentPrefix = %q", got)
	}
}
