package runtime

import "testing"

// TestFilterEnv — managed project processes must not inherit Lambs's own
// credential surface (COMPUTE_AGENT_TOKEN in project code = SYSTEM /cmd on
// another machine, R24). Regression guard for the blocklist.
func TestFilterEnv(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			"credentials stripped",
			[]string{
				"PATH=/usr/bin",
				"DATABASE_URL=postgres://x",
				"JWT_SECRET=abc",
				"COMPUTE_AGENT_TOKEN=tok",
				"COMPUTE_AGENT_URL=http://a",
				"WOOL_AGENT_URL=http://b",
				"TG_BOT_TOKEN=tg",
				"SMTP_PASSWORD=pw",
				"GITHUB_TOKEN=gh",
				"CLOUDFLARE_API_TOKEN=cf",
				"LAMBS_CONFIG_PATH=/etc/lambs",
				"PORT=3602",
				"HOME=/home/ubuntu",
			},
			[]string{"PATH=/usr/bin", "PORT=3602", "HOME=/home/ubuntu"},
		},
		{
			"prefix boundary: LAMBSERVER is not LAMBS_",
			[]string{"LAMBSERVER=1", "LAMBS_X=2", "TG_=3", "TGSTUFF=4"},
			[]string{"LAMBSERVER=1", "TGSTUFF=4"},
		},
		{
			"empty env",
			[]string{},
			[]string{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := filterEnv(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("filterEnv(%v) = %v, want %v", c.in, got, c.want)
			}
			wantSet := map[string]bool{}
			for _, w := range c.want {
				wantSet[w] = true
			}
			for _, g := range got {
				if !wantSet[g] {
					t.Errorf("unexpected kept entry %q (got %v)", g, got)
				}
			}
		})
	}
}
