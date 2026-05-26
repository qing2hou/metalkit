package installer

import (
	"strings"
	"testing"
)

func TestResolveBlobURL(t *testing.T) {
	cases := []struct {
		name    string
		base    string
		rel     string
		want    string
		wantErr string
	}{
		{
			name: "relative path with trailing-slash base",
			base: "http://10.0.0.1:8080/",
			rel:  "/api/v1/images/abc/blob",
			want: "http://10.0.0.1:8080/api/v1/images/abc/blob",
		},
		{
			name: "relative path with no trailing-slash base",
			base: "http://10.0.0.1:8080",
			rel:  "/api/v1/images/abc/blob",
			want: "http://10.0.0.1:8080/api/v1/images/abc/blob",
		},
		{
			name: "absolute rel ignores base",
			base: "http://10.0.0.1:8080",
			rel:  "http://other.example/x/y",
			want: "http://other.example/x/y",
		},
		{
			name: "relative without leading slash gets prefixed",
			base: "http://10.0.0.1:8080",
			rel:  "api/v1/images/abc/blob",
			want: "http://10.0.0.1:8080/api/v1/images/abc/blob",
		},
		{
			name:    "empty rel errors",
			base:    "http://10.0.0.1:8080",
			rel:     "",
			wantErr: "image_blob_url is empty",
		},
		{
			name:    "relative rel with empty base errors",
			base:    "",
			rel:     "/api/v1/x",
			wantErr: "BaseURL is empty",
		},
		{
			name:    "base without scheme errors",
			base:    "10.0.0.1:8080",
			rel:     "/api/v1/x",
			wantErr: "base url",
		},
		{
			name: "base with path prefix is replaced by absolute rel path",
			base: "http://10.0.0.1:8080/controller",
			rel:  "/api/v1/x",
			want: "http://10.0.0.1:8080/api/v1/x",
		},
		{
			name: "https base",
			base: "https://controller.metalkit.local",
			rel:  "/api/v1/images/blob",
			want: "https://controller.metalkit.local/api/v1/images/blob",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveBlobURL(tc.base, tc.rel)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil (val=%q)", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err %q does not contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}
