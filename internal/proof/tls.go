package proof

import "crypto/tls"

// insecureTLS turns off the certificate check, and says why here where it can be seen: every
// message is sealed to a key the two sides already proved to each other. A reverse proxy's
// certificate, if it has one, is not what makes this safe, and a self-signed one must work.
func insecureTLS() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true} //nolint:gosec
}
