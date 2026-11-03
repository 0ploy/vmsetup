package machine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/0ploy/vmsetup/internal/config"
)

func cfg(command []string, exec *config.Exec) *config.Config {
	c := &config.Config{Exec: exec}
	c.Handoff.Command = command
	if command != nil {
		c.Handoff.User = "nginx"
		c.Handoff.WorkingDir = "/"
	}
	c.Handoff.StopSignal = "SIGQUIT"
	c.Handoff.StopTimeout = config.Duration(10 * time.Second)
	return c
}

func TestFromConfigWrap(t *testing.T) {
	env := []string{"PATH=/bin", "HOME=/home/nginx"}
	s := FromConfig(cfg([]string{"nginx"}, &config.Exec{Port: 1024, User: "app", WorkingDir: "/srv"}), "/dev/ttyS0", env)
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"v":1,"console":"/dev/ttyS0","stop":{"signal":"SIGQUIT","timeout":"10s"},"user":"nginx",` +
		`"exec":{"port":1024,"user":"app","working_dir":"/srv","env":["PATH=/bin","HOME=/home/nginx"]}}`
	if string(b) != want {
		t.Errorf("got  %s\nwant %s", b, want)
	}
}

func TestFromConfigSupervise(t *testing.T) {
	s := FromConfig(cfg(nil, nil), "/dev/ttyS0", nil)
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), `"user"`) || strings.Contains(string(b), `"exec"`) || strings.Contains(string(b), `"rescue"`) {
		t.Errorf("supervise mode with no exec must carry no user, exec or rescue: %s", b)
	}
}

func TestEnvFile(t *testing.T) {
	content, skipped := EnvFile([]string{"A=1", "MULTI=x\ny", "BAD-KEY=v", "EMPTY=", "B=two words"})
	want := "# written by vmsetup; zpinit's entrypoint.d scripts may append\nA=1\nEMPTY=\nB=two words\n"
	if content != want {
		t.Errorf("content = %q", content)
	}
	if strings.Join(skipped, ",") != "MULTI,BAD-KEY" {
		t.Errorf("skipped = %v", skipped)
	}
}
