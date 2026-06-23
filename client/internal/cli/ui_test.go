package cli

import "testing"

func TestShortVolume(t *testing.T) {
	hash := "0319f545b2fd31e9fe03382447d1c1235c6cef40a6c527d8b3e55ad7cfc56af2"
	if got, want := shortVolume(hash), hash[:12]+"…"; got != want {
		t.Errorf("shortVolume(hash) = %q, want %q", got, want)
	}
	for _, named := range []string{"consul_consul-data", "nextcloud_harp-certs", "data", "abc123"} {
		if got := shortVolume(named); got != named {
			t.Errorf("shortVolume(%q) = %q, want unchanged", named, got)
		}
	}
}
