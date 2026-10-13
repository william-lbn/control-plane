package control

import (
	"errors"
	"net/url"
	"os"
	"regexp"
)

func validateS3Config() error {
	if s3Origin() == "" {
		return nil
	}
	u, e := url.Parse(s3Origin())
	if e != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && os.Getenv("NEON_S3_LAB_HTTP") == "true")) || !regexp.MustCompile(`^[a-z0-9-]{1,32}$`).MatchString(s3Region()) {
		return errors.New("explicit S3 origin/region/trust required")
	}
	return nil
}
