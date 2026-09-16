package template

import "testing"

// A build authenticates only to the registries of its own tenant. The
// operator's Docker credentials are deliberately absent, so a tenant cannot
// borrow them to reach a registry the operator can reach.
func TestBuilderHostsCarryOnlyItsOwnCredentials(t *testing.T) {
	b := Builder{Credentials: []RegistryCredential{
		{Host: "ghcr.io", Username: "acme-bot", Token: "acme-token"},
	}}
	hosts := b.hosts()
	if len(hosts) != 1 {
		t.Fatalf("hosts %d, want 1", len(hosts))
	}
	if hosts[0].Name != "ghcr.io" || hosts[0].User != "acme-bot" || hosts[0].Pass != "acme-token" {
		t.Fatalf("host %+v does not carry the credential it was given", hosts[0])
	}

	// A tenant with none pulls anonymously. An empty list is not the
	// operator's credentials by another name.
	empty := Builder{}
	if n := len(empty.hosts()); n != 0 {
		t.Fatalf("a build with no credentials offers %d hosts, want none", n)
	}
}
