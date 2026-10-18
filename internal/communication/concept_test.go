package communication

import (
	"strings"
	"testing"
)

func names(t *testing.T) ([]string, error) {
	t.Helper()
	ps, err := Configured(nil)
	var out []string
	for _, p := range ps {
		out = append(out, p.Name())
	}
	return out, err
}

func TestConfigured(t *testing.T) {
	tests := []struct {
		name      string
		env       map[string]string
		want      string
		wantError string
	}{
		{name: "nothing configured", want: ""},
		{
			name: "telegram only",
			env:  map[string]string{"TELEGRAM_BOT_TOKEN": "t", "TELEGRAM_CHAT_ID": "1"},
			want: "communication/telegram",
		},
		{
			name: "both channels",
			env: map[string]string{
				"TELEGRAM_BOT_TOKEN": "t", "TELEGRAM_CHAT_ID": "1",
				"SLACK_WEBHOOK_URL": "https://hooks.slack.com/services/T/B/X",
			},
			want: "communication/telegram,communication/slack",
		},
		{
			name:      "half-configured telegram is an error, slack still built",
			env:       map[string]string{"TELEGRAM_CHAT_ID": "1", "SLACK_WEBHOOK_URL": "https://hooks.slack.com/services/T/B/X"},
			want:      "communication/slack",
			wantError: "TELEGRAM_BOT_TOKEN",
		},
		{
			name:      "plain-http slack webhook is an error",
			env:       map[string]string{"SLACK_WEBHOOK_URL": "http://hooks.slack.com/services/T/B/X"},
			wantError: "https",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, k := range []string{"TELEGRAM_BOT_TOKEN", "TELEGRAM_CHAT_ID", "SLACK_WEBHOOK_URL"} {
				t.Setenv(k, tt.env[k])
			}
			got, err := names(t)
			if strings.Join(got, ",") != tt.want {
				t.Errorf("providers %v, want %q", got, tt.want)
			}
			switch {
			case tt.wantError == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tt.wantError != "" && (err == nil || !strings.Contains(err.Error(), tt.wantError)):
				t.Errorf("error %v, want one mentioning %q", err, tt.wantError)
			}
		})
	}
}
