package aws

import (
	"bytes"
	"net/http"
	"testing"
	"time"
)

// The cases come from the AWS Signature Version 4 test suite, as botocore
// ships it in tests/unit/auth/aws4_testsuite.
func TestSignerMatchesTheAWSTestSuite(t *testing.T) {
	const token = "AQoDYXdzEPT//////////wEXAMPLEtc764bNrC9SAPBSM22wDOk4x4HIZ8j4FZTwdQWLWsKWHGBuFqwAeMicRXmxfpSPfIeoIYRqTflfKD8YUuwthAx7mSEI/qkPpKPi/kMcGdQrmGdeehM4IC1NtBmUpp2wUE8phUZampKsburEDy0KPkyQDYwT7WZ0wq5VSXDvp75YU9HFvlRd8Tx6q6fE8YQcHNVXAkiY9q6d+xo0rKwT38xVqr7ZD0u0iPPkUL64lIZbqBAz+scqKmlzm8FDrypNC9Yjc8fPOLn9FX9KSYvKTr4rvx3iSIlTJabIQwj2ICCR/oLxBA=="
	for name, tc := range map[string]struct {
		body         string
		contentType  string
		sessionToken string
		want         string
	}{
		"post-x-www-form-urlencoded": {
			body: "Param1=value1", contentType: "application/x-www-form-urlencoded",
			want: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=ff11897932ad3f4e8b18135d722051e5ac45fc38421b1da7b9d196a0fe09473a",
		},
		"post-sts-header-before": {
			sessionToken: token,
			want:         "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date;x-amz-security-token, Signature=85d96828115b5dc0cfc3bd16ad9e210dd772bbebba041836c64533a82be05ead",
		},
	} {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.amazonaws.com/", bytes.NewReader([]byte(tc.body)))
			if err != nil {
				t.Fatal(err)
			}
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			s := signer{
				creds:  Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", SessionToken: tc.sessionToken},
				region: "us-east-1", service: "service",
				now: func() time.Time { return time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC) },
			}

			s.sign(req, []byte(tc.body))

			if got := req.Header.Get("Authorization"); got != tc.want {
				t.Fatalf("Authorization = %s\nwant            %s", got, tc.want)
			}
			if req.Header.Get("X-Amz-Date") != "20150830T123600Z" || req.Header.Get("X-Amz-Security-Token") != tc.sessionToken {
				t.Fatalf("X-Amz-Date = %q, X-Amz-Security-Token = %q", req.Header.Get("X-Amz-Date"), req.Header.Get("X-Amz-Security-Token"))
			}
		})
	}
}
