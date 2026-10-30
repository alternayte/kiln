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

// A build's own login replaces the stored one on its registry only, so a job
// token never reaches another registry.
func TestBuildCredentialReplacesOnlyItsRegistry(t *testing.T) {
	stored := []RegistryCredential{
		{Host: "ghcr.io", Username: "bot", Token: "stored"},
		{Host: "registry.example.com", Username: "other", Token: "other"},
	}
	got, err := withBuildCredential(stored, "ghcr.io/acme/app:1", &Credential{Username: "job", Token: "job-token"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	for _, c := range got {
		switch c.Host {
		case "ghcr.io":
			if c.Token != "job-token" {
				t.Errorf("ghcr.io uses %q", c.Token)
			}
		case "registry.example.com":
			if c.Token != "other" {
				t.Errorf("the other registry uses %q", c.Token)
			}
		default:
			t.Errorf("unexpected host %q", c.Host)
		}
	}
	same, err := withBuildCredential(stored, "ghcr.io/acme/app:1", nil)
	if err != nil || len(same) != 2 || same[0].Token != "stored" {
		t.Fatalf("no credential changed the stored ones: %+v, %v", same, err)
	}
}
