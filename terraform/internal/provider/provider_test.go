package provider

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const testToken = "terraform-acceptance-admin-token-000000"

var factories = map[string]func() (tfprotov6.ProviderServer, error){
	"genaiopengateway": providerserver.NewProtocol6WithError(New("test")()),
}

// gateway builds and starts a gateway on SQLite and returns its URL.
// Acceptance tests run with TF_ACC=1 and need the terraform CLI.
func gateway(t *testing.T) string {
	t.Helper()
	if os.Getenv("TF_ACC") == "" {
		t.Skip("acceptance tests run with TF_ACC=1")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "gateway")
	build := exec.Command("go", "build", "-o", bin, "./cmd/gateway")
	build.Dir = ".."
	for ; ; build.Dir = filepath.Join(build.Dir, "..") {
		if _, err := os.Stat(filepath.Join(build.Dir, "cmd", "gateway")); err == nil {
			break
		}
		if abs, _ := filepath.Abs(build.Dir); abs == "/" {
			t.Fatal("cmd/gateway not found above the provider")
		}
	}
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the gateway: %v\n%s", err, out)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	cfg := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf(`listen: %q
metrics: {listen: "127.0.0.1:0"}
admin: {token: %s}
store: {driver: sqlite, dsn: "file:%s"}
secrets: {encryption_key: "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="}
providers: {up: {base_url: "http://127.0.0.1:9/v1", api_key: x}}
models: {chat: {targets: [{provider: up, model: m}]}}
`, addr, testToken, filepath.Join(dir, "g.db"))), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-config", cfg)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	url := "http://" + addr
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if resp, err := http.Get(url + "/readyz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return url
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the gateway did not become ready")
		}
	}
}

func TestAccResources(t *testing.T) {
	url := gateway(t)
	provider := fmt.Sprintf("provider \"genaiopengateway\" {\n  url = %q\n  token = %q\n}\n", url, testToken)
	step1 := provider + `
resource "genaiopengateway_organization" "acme" {
  name = "acme"
  rpm  = 100
  budget = { amount_usd = 50, period = "monthly" }
}

resource "genaiopengateway_team" "search" {
  org_id         = genaiopengateway_organization.acme.id
  name           = "search"
  allowed_models = ["chat"]
  cache          = { mode = "exact", ttl = "1h" }
}

resource "genaiopengateway_key" "ci" {
  team_id = genaiopengateway_team.search.id
  name    = "ci"
  owner   = "ci@example.com"
  tpm     = 5000
}

resource "genaiopengateway_cedar_policies" "acme" {
  org_id = genaiopengateway_organization.acme.id
  text   = <<-EOT
    @id("base")
    permit (principal, action, resource);
  EOT
}

resource "genaiopengateway_config_object" "fast" {
  kind = "models"
  name = "fast"
  spec = jsonencode({ targets = [{ provider = "up", model = "fast" }] })
}

resource "genaiopengateway_config_object" "other" {
  kind = "providers"
  name = "other"
  spec = jsonencode({ type = "openai", base_url = "http://127.0.0.1:9/v1", api_key = "secret-value" })
}

data "genaiopengateway_organization" "acme" {
  name = genaiopengateway_organization.acme.name
}
`
	step2 := provider + `
resource "genaiopengateway_organization" "acme" {
  name = "acme-renamed"
}

resource "genaiopengateway_team" "search" {
  org_id = genaiopengateway_organization.acme.id
  name   = "search"
}

resource "genaiopengateway_key" "ci" {
  team_id = genaiopengateway_team.search.id
  name    = "ci-2"
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: step1,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestMatchResourceAttr("genaiopengateway_organization.acme", "id", regexp.MustCompile(`^org_`)),
					resource.TestCheckResourceAttr("genaiopengateway_organization.acme", "budget.amount_usd", "50"),
					resource.TestCheckResourceAttr("genaiopengateway_team.search", "allowed_models.0", "chat"),
					resource.TestMatchResourceAttr("genaiopengateway_key.ci", "secret", regexp.MustCompile(`^sk-gog-`)),
					resource.TestCheckResourceAttr("genaiopengateway_cedar_policies.acme", "version", "1"),
					resource.TestCheckResourceAttrPair("data.genaiopengateway_organization.acme", "id", "genaiopengateway_organization.acme", "id"),
					keyWorks(url, "genaiopengateway_key.ci"),
				),
			},
			{ResourceName: "genaiopengateway_organization.acme", ImportState: true, ImportStateVerify: true},
			{ResourceName: "genaiopengateway_team.search", ImportState: true, ImportStateVerify: true},
			{ResourceName: "genaiopengateway_cedar_policies.acme", ImportState: true, ImportStateVerify: true},
			{ResourceName: "genaiopengateway_key.ci", ImportState: true, ImportStateVerify: true, ImportStateVerifyIgnore: []string{"secret"}},
			{
				Config: step2,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("genaiopengateway_organization.acme", "name", "acme-renamed"),
					resource.TestCheckNoResourceAttr("genaiopengateway_organization.acme", "rpm"),
					resource.TestCheckNoResourceAttr("genaiopengateway_team.search", "allowed_models"),
					resource.TestCheckResourceAttr("genaiopengateway_key.ci", "name", "ci-2"),
				),
			},
		},
	})
}

// keyWorks checks that a key's secret authenticates with the gateway.
func keyWorks(url, name string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[name]
		req, _ := http.NewRequest(http.MethodGet, url+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+rs.Primary.Attributes["secret"])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("the key's secret got %d", resp.StatusCode)
		}
		return nil
	}
}
