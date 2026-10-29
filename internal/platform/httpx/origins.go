package httpx

import "strings"

// Origins is a set of exact browser origins (scheme://host[:port]), as used
// by CORS_ALLOWED_ORIGINS. The zero value and nil allow nothing.
type Origins struct {
	set  map[string]struct{}
	list []string
}

func NewOrigins(origins []string) *Origins {
	o := &Origins{set: make(map[string]struct{}, len(origins))}
	for _, raw := range origins {
		n := NormalizeOrigin(raw)
		if n == "" {
			continue
		}
		if _, dup := o.set[n]; !dup {
			o.set[n] = struct{}{}
			o.list = append(o.list, n)
		}
	}
	return o
}

// Allowed reports whether origin is in the set.
func (o *Origins) Allowed(origin string) bool {
	if o == nil {
		return false
	}
	_, ok := o.set[NormalizeOrigin(origin)]
	return ok
}

// List returns the normalized origins in their original order.
func (o *Origins) List() []string {
	if o == nil {
		return nil
	}
	return append([]string(nil), o.list...)
}

// NormalizeOrigin lower-cases and trims an origin and drops a trailing slash,
// so "https://App.example.com/" matches the browser's "https://app.example.com".
func NormalizeOrigin(origin string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(origin)), "/")
}
