package probe

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestValidateHost(t *testing.T) {
	for _, ok := range []string{"example.com", "192.168.1.10", "2001:db8::1", "web-1", "my_host"} {
		if err := ValidateHost(ok); err != nil {
			t.Errorf("ValidateHost(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-bad", "bad host", "host;rm -rf /", "$(id)", "a/b"} {
		if err := ValidateHost(bad); err == nil {
			t.Errorf("ValidateHost(%q) should fail", bad)
		}
	}
}

func TestValidateURL(t *testing.T) {
	for _, ok := range []string{"http://example.com", "https://example.com/path?q=1"} {
		if err := ValidateURL(ok); err != nil {
			t.Errorf("ValidateURL(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "example.com", "ftp://example.com", "http://", "javascript:alert(1)"} {
		if err := ValidateURL(bad); err == nil {
			t.Errorf("ValidateURL(%q) should fail", bad)
		}
	}
}

func TestBuildCommands(t *testing.T) {
	if got := BuildPing("example.com"); !strings.Contains(got, "ping -c 4") || !strings.Contains(got, "'example.com'") {
		t.Errorf("BuildPing = %q", got)
	}
	if got := BuildIperfClient("host", 5201, 5); !strings.Contains(got, "-c 'host'") || !strings.Contains(got, "-p 5201") || !strings.Contains(got, "-J") {
		t.Errorf("BuildIperfClient = %q", got)
	}
	if got := BuildIperfServer(5201); got != "iperf3 -s -1 -p 5201" {
		t.Errorf("BuildIperfServer = %q", got)
	}
}

func TestProbeShellSyntax(t *testing.T) {
	for name, script := range map[string]string{
		"dns":        BuildDNS("example.com"),
		"traceroute": BuildTraceroute("example.com"),
	} {
		cmd := exec.Command("sh", "-n")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s syntax: %v (%s)", name, err, out)
		}
	}
}

func TestParsePing(t *testing.T) {
	out := `PING example.com (93.184.216.34) 56(84) bytes of data.
64 bytes from 93.184.216.34: icmp_seq=1 ttl=56 time=10.1 ms
64 bytes from 93.184.216.34: icmp_seq=2 ttl=56 time=11.3 ms

--- example.com ping statistics ---
2 packets transmitted, 1 received, 50% packet loss, time 1002ms
rtt min/avg/max/mdev = 10.100/10.700/11.300/0.600 ms`
	d, err := ParsePing(out)
	if err != nil {
		t.Fatalf("ParsePing: %v", err)
	}
	if d.Transmitted != 2 || d.Received != 1 || d.LossPct != 50 || d.AvgMS != 10.7 {
		t.Fatalf("ping data = %+v", d)
	}
	if _, err := ParsePing("nothing"); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestParseDNS(t *testing.T) {
	got := ParseDNS("93.184.216.34  example.com\n\n2001:db8::1  example.com\n")
	if len(got) != 2 || got[0] != "93.184.216.34  example.com" {
		t.Fatalf("records = %v", got)
	}
}

func TestParseHTTP(t *testing.T) {
	d, err := ParseHTTP(`{"code":204,"time":0.123,"size":0}`)
	if err != nil {
		t.Fatalf("ParseHTTP: %v", err)
	}
	if d.Code != 204 || d.TotalMS != 123 || d.Size != 0 {
		t.Fatalf("http data = %+v", d)
	}
	if _, err := ParseHTTP("not json"); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestParseTraceroute(t *testing.T) {
	out := `traceroute to example.com (93.184.216.34), 20 hops max
 1  192.168.1.1  1.123 ms
 2  * * *
 3  203.0.113.1  12.456 ms`
	if got := ParseTraceroute(out); got != 3 {
		t.Fatalf("hops = %d, want 3", got)
	}
}

func TestParseIperf3(t *testing.T) {
	out := `{"end":{"sum_sent":{"bits_per_second":941234567.0},"sum_received":{"bits_per_second":940000000.0}}}`
	d, err := ParseIperf3(out)
	if err != nil {
		t.Fatalf("ParseIperf3: %v", err)
	}
	if d.SentMbps < 941 || d.SentMbps > 942 || d.RecvMbps < 939 || d.RecvMbps > 941 {
		t.Fatalf("iperf data = %+v", d)
	}
	if _, err := ParseIperf3(`{"end":{}}`); err == nil {
		t.Fatal("expected parse error")
	}
}

type fakeRunner struct {
	out string
	err error
	cmd string
}

func (f *fakeRunner) RunCommand(_ context.Context, cmd string) (string, error) {
	f.cmd = cmd
	return f.out, f.err
}

func TestRunPingAndFailure(t *testing.T) {
	r := &fakeRunner{out: "4 packets transmitted, 4 received, 0% packet loss\nrtt min/avg/max/mdev = 1.0/2.0/3.0/0.1 ms"}
	res := Run(context.Background(), r, Request{Type: TypePing, Target: "example.com"})
	if res.Status != "success" || !strings.Contains(res.Summary, "4/4") {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(r.cmd, "example.com") {
		t.Fatalf("command = %q", r.cmd)
	}

	r = &fakeRunner{err: errors.New("boom")}
	res = Run(context.Background(), r, Request{Type: TypeDNS, Target: "example.com"})
	if res.Status != "error" || res.Error != "boom" {
		t.Fatalf("failure result = %+v", res)
	}
}

func TestRunHTTP(t *testing.T) {
	r := &fakeRunner{out: `{"code":200,"time":0.05,"size":10}`}
	res := Run(context.Background(), r, Request{Type: TypeHTTP, Target: "https://example.com"})
	if res.Status != "success" || !strings.Contains(res.Summary, "HTTP 200") {
		t.Fatalf("result = %+v", res)
	}
}
