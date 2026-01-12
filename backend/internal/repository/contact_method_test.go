package repository

import "testing"

func TestNormalizeContactMethodValue(t *testing.T) {
	cases := []struct {
		name       string
		methodType string
		value      string
		expected   string
	}{
		{
			name:       "email lowercases",
			methodType: string(ContactMethodEmailPersonal),
			value:      " Person@Example.com ",
			expected:   "person@example.com",
		},
		{
			name:       "phone e164",
			methodType: string(ContactMethodPhone),
			value:      "+1 (555) 123-4567",
			expected:   "+15551234567",
		},
		{
			name:       "telegram handle",
			methodType: string(ContactMethodTelegram),
			value:      " @Handle ",
			expected:   "handle",
		},
		{
			name:       "discord handle",
			methodType: string(ContactMethodDiscord),
			value:      "@DiscordUser",
			expected:   "discorduser",
		},
		{
			name:       "twitter handle",
			methodType: string(ContactMethodTwitter),
			value:      "  @TwitterUser  ",
			expected:   "twitteruser",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			actual := NormalizeContactMethodValue(tt.methodType, tt.value)
			if actual != tt.expected {
				t.Fatalf("expected %q, got %q", tt.expected, actual)
			}
		})
	}
}
