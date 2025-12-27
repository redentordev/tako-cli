package acmedns

import (
	"time"
)

// Registration represents an acme-dns registration for a domain
type Registration struct {
	Domain      string    `json:"domain"`       // Base domain (e.g., "example.com")
	Subdomain   string    `json:"subdomain"`    // acme-dns subdomain
	Username    string    `json:"username"`     // acme-dns update username
	Password    string    `json:"password"`     // acme-dns update password
	FullDomain  string    `json:"fulldomain"`   // Full acme-dns domain
	CNAMETarget string    `json:"cname_target"` // Target for user's CNAME record
	ServerIP    string    `json:"server_ip"`    // IP of the server running acme-dns
	CreatedAt   time.Time `json:"created_at"`
}

// PendingCertificate represents a certificate waiting for DNS propagation
type PendingCertificate struct {
	Domain       string        `json:"domain"`
	Registration *Registration `json:"registration"`
	StartedAt    time.Time     `json:"started_at"`
	LastCheck    time.Time     `json:"last_check"`
	Attempts     int           `json:"attempts"`
}

// Credentials stores acme-dns credentials for LEGO
type Credentials struct {
	Registrations map[string]*Registration `json:"registrations"` // Keyed by base domain
}
