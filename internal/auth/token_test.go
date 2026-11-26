package auth

import "testing"

func TestResolveToken(t *testing.T) {
	tests := []struct {
		name, flag, env string
		want            string
		wantErr         bool
	}{
		{name: "flag", flag: "s3cret", want: "s3cret"},
		{name: "env fallback", env: "fromenv", want: "fromenv"},
		{name: "flag wins", flag: "a", env: "b", want: "a"},
		{name: "missing", wantErr: true},
		{name: "placeholder flag", flag: InsecureDefault, wantErr: true},
		{name: "placeholder env", env: InsecureDefault, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvToken, tt.env)
			got, err := ResolveToken(tt.flag)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("ResolveToken() = %q, %v; want %q, err=%v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}
