package httpapi

import (
	"net/http"
	"testing"
)

func TestRoleCanAccessRequest(t *testing.T) {
	tests := []struct {
		name   string
		role   string
		method string
		path   string
		want   bool
	}{
		{name: "admin may create domain", role: roleAdmin, method: http.MethodPost, path: "/api/v1/domains", want: true},
		{name: "readonly may search", role: roleReadonly, method: http.MethodGet, path: "/api/v1/search", want: true},
		{name: "readonly may view certificates", role: roleReadonly, method: http.MethodGet, path: "/api/v1/certificates", want: true},
		{name: "readonly may logout", role: roleReadonly, method: http.MethodPost, path: "/api/v1/auth/logout", want: true},
		{name: "readonly cannot change password", role: roleReadonly, method: http.MethodPost, path: "/api/v1/auth/password"},
		{name: "readonly cannot create domain", role: roleReadonly, method: http.MethodPost, path: "/api/v1/domains"},
		{name: "readonly cannot bulk create domains", role: roleReadonly, method: http.MethodPost, path: "/api/v1/domains/bulk"},
		{name: "readonly cannot edit domain", role: roleReadonly, method: http.MethodPatch, path: "/api/v1/domains/507f1f77bcf86cd799439011"},
		{name: "readonly cannot archive domain", role: roleReadonly, method: http.MethodDelete, path: "/api/v1/domains/507f1f77bcf86cd799439011"},
		{name: "readonly cannot collect domain", role: roleReadonly, method: http.MethodPost, path: "/api/v1/domains/507f1f77bcf86cd799439011/collect"},
		{name: "readonly cannot collect all", role: roleReadonly, method: http.MethodPost, path: "/api/v1/collect"},
		{name: "readonly cannot refresh certificates", role: roleReadonly, method: http.MethodPost, path: "/api/v1/certificates/refresh"},
		{name: "unknown role is denied", role: "unknown", method: http.MethodGet, path: "/api/v1/search"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := roleCanAccessRequest(tt.role, tt.method, tt.path); got != tt.want {
				t.Fatalf("roleCanAccessRequest(%q, %q, %q) = %t, want %t", tt.role, tt.method, tt.path, got, tt.want)
			}
		})
	}
}
