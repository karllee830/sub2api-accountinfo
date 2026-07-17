package main

import "testing"

func TestLoadConfigAllowReset(t *testing.T) {
	testCases := []struct {
		name      string
		value     string
		want      bool
		wantError bool
	}{
		{name: "default disabled"},
		{name: "explicitly disabled", value: "false"},
		{name: "globally enabled", value: "true", want: true},
		{name: "invalid", value: "enabled", wantError: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("SUB2API_URL", "https://sub2api.example")
			t.Setenv("SUB2API_ADMIN_API_KEY", "admin-test-key")
			t.Setenv("ALLOW_RESET", testCase.value)
			t.Setenv("TRUST_PROXY_HEADERS", "true")
			t.Setenv("FRAME_ANCESTORS", "")
			t.Setenv("LISTEN_ADDR", "")

			cfg, err := loadConfig()
			if testCase.wantError {
				if err == nil {
					t.Fatal("loadConfig() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.allowReset != testCase.want {
				t.Fatalf("allowReset = %t, want %t", cfg.allowReset, testCase.want)
			}
		})
	}
}
