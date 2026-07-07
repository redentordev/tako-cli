package engine

// KindProxyHashPasswordResult identifies the machine-readable result document
// for `tako proxy hash-password`.
const KindProxyHashPasswordResult = "ProxyHashPasswordResult"

// ProxyHashPasswordResult carries the bcrypt hash minted for
// proxy.basicAuth.passwordBcrypt. The plaintext password is never included.
type ProxyHashPasswordResult struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Cost       int    `json:"cost"`
	Hash       string `json:"hash"`
}
