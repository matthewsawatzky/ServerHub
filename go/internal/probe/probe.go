// Package probe builds and parses small network diagnostics that run as SSH
// exec commands on a server's control connection. No remote agent is required.
package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	TypePing       = "ping"
	TypeDNS        = "dns"
	TypeHTTP       = "http"
	TypeTraceroute = "traceroute"
	TypeIperf3     = "iperf3"

	maxOutput = 16 << 10
)

var (
	hostPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,252})$`)
	lossRe      = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)%\s*packet loss`)
	rttRe       = regexp.MustCompile(`(?:rtt|round-trip) min/avg/max(?:/mdev)? = ([0-9.]+)/([0-9.]+)/([0-9.]+)`)
	hopRe       = regexp.MustCompile(`(?m)^\s*([0-9]+)\s`)
)

// Runner is the subset of *ssh.ServerEntry the probes need.
type Runner interface {
	RunCommand(ctx context.Context, cmd string) (string, error)
}

// Request is one probe invocation.
type Request struct {
	Type   string `json:"type"`
	Target string `json:"target"`
}

// PingData is the parsed summary of `ping -c 4`.
type PingData struct {
	Transmitted int     `json:"transmitted"`
	Received    int     `json:"received"`
	LossPct     float64 `json:"loss_pct"`
	MinMS       float64 `json:"min_ms,omitempty"`
	AvgMS       float64 `json:"avg_ms,omitempty"`
	MaxMS       float64 `json:"max_ms,omitempty"`
}

// IperfData is the throughput summary from `iperf3 -J`.
type IperfData struct {
	SentBPS     float64 `json:"sent_bps"`
	ReceivedBPS float64 `json:"received_bps"`
	SentMbps    float64 `json:"sent_mbps"`
	RecvMbps    float64 `json:"received_mbps"`
	Port        int     `json:"port,omitempty"`
}

// Result is rendered inline by the UI and is never executed as markup.
type Result struct {
	Type       string                 `json:"type"`
	Target     string                 `json:"target"`
	Status     string                 `json:"status"` // success | error
	StartedAt  int64                  `json:"started_at"`
	DurationMS int64                  `json:"duration_ms"`
	Summary    string                 `json:"summary"`
	Error      string                 `json:"error,omitempty"`
	Data       map[string]interface{} `json:"data,omitempty"`
	Output     string                 `json:"output,omitempty"`
}

// NormalizeType lowercases and validates a probe type.
func NormalizeType(t string) (string, error) {
	t = strings.ToLower(strings.TrimSpace(t))
	switch t {
	case TypePing, TypeDNS, TypeHTTP, TypeTraceroute, TypeIperf3:
		return t, nil
	default:
		return "", fmt.Errorf("unknown probe type %q", t)
	}
}

// ValidateHost accepts an IP address or a conservative DNS name.
func ValidateHost(target string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		return fmt.Errorf("target is required")
	}
	if len(target) > 253 {
		return fmt.Errorf("target is too long")
	}
	if net.ParseIP(target) != nil {
		return nil
	}
	if !hostPattern.MatchString(target) {
		return fmt.Errorf("target must be an IP address or hostname")
	}
	return nil
}

// ValidateURL accepts http/https URLs only.
func ValidateURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 {
		return fmt.Errorf("URL is required and must be at most 2048 characters")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL must start with http:// or https://")
	}
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// BuildPing returns the remote command for a four-packet ping.
func BuildPing(target string) string {
	return fmt.Sprintf("ping -c 4 -W 2 -w 10 %s", shellQuote(target))
}

// BuildDNS resolves a host using the first available tool.
func BuildDNS(target string) string {
	script := fmt.Sprintf(`if command -v getent >/dev/null 2>&1; then
  getent hosts %s
elif command -v dig >/dev/null 2>&1; then
  dig +short %s
elif command -v nslookup >/dev/null 2>&1; then
  nslookup %s
else
  echo "no DNS resolver tool found (getent/dig/nslookup)" >&2
  exit 127
fi`, shellQuote(target), shellQuote(target), shellQuote(target))
	return "/bin/sh -c " + shellQuote(script)
}

// BuildHTTP times a HEAD/GET-style request without downloading the body.
func BuildHTTP(rawURL string) string {
	format := `{"code":%{http_code},"time":%{time_total},"size":%{size_download}}`
	return fmt.Sprintf("curl -sS -o /dev/null --max-time 10 -w %s %s",
		shellQuote(format), shellQuote(rawURL))
}

// BuildTraceroute prefers traceroute and falls back to mtr.
func BuildTraceroute(target string) string {
	script := fmt.Sprintf(`if command -v traceroute >/dev/null 2>&1; then
  traceroute -n -w 2 -m 20 %s
elif command -v mtr >/dev/null 2>&1; then
  mtr -r -c 5 -n %s
else
  echo "traceroute/mtr is not installed" >&2
  exit 127
fi`, shellQuote(target), shellQuote(target))
	return "/bin/sh -c " + shellQuote(script)
}

// BuildIperfClient builds the JSON-mode client command.
func BuildIperfClient(host string, port int, seconds int) string {
	if seconds <= 0 {
		seconds = 5
	}
	return fmt.Sprintf("iperf3 -c %s -p %d -t %d -J", shellQuote(host), port, seconds)
}

// BuildIperfServer builds the one-shot server command.
func BuildIperfServer(port int) string {
	return fmt.Sprintf("iperf3 -s -1 -p %d", port)
}

// Run executes a non-iperf probe and returns a result even on command failure.
func Run(ctx context.Context, runner Runner, req Request) Result {
	start := time.Now()
	res := Result{Type: req.Type, Target: strings.TrimSpace(req.Target), StartedAt: start.Unix()}

	cmd, err := commandFor(req)
	if err != nil {
		res.Status = "error"
		res.Error = err.Error()
		res.Summary = err.Error()
		return res
	}
	out, runErr := runner.RunCommand(ctx, cmd)
	res.Output = capOutput(out)
	res.DurationMS = time.Since(start).Milliseconds()
	if runErr != nil {
		res.Status = "error"
		res.Error = runErr.Error()
		res.Summary = "probe failed"
		return res
	}

	switch req.Type {
	case TypePing:
		data, err := ParsePing(out)
		if err != nil {
			res.Status = "error"
			res.Error = err.Error()
			res.Summary = "could not parse ping output"
			return res
		}
		res.Status = "success"
		res.Data = map[string]interface{}{"ping": data}
		res.Summary = fmt.Sprintf("%d/%d received, %.1f%% loss",
			data.Received, data.Transmitted, data.LossPct)
		if data.AvgMS > 0 {
			res.Summary += fmt.Sprintf(", avg %.1f ms", data.AvgMS)
		}
	case TypeDNS:
		records := ParseDNS(out)
		res.Status = "success"
		res.Data = map[string]interface{}{"records": records}
		if len(records) == 0 {
			res.Summary = "no records returned"
		} else {
			res.Summary = fmt.Sprintf("%d record(s): %s", len(records), records[0])
		}
	case TypeHTTP:
		data, err := ParseHTTP(out)
		if err != nil {
			res.Status = "error"
			res.Error = err.Error()
			res.Summary = "could not parse curl output"
			return res
		}
		res.Status = "success"
		res.Data = map[string]interface{}{"http": data}
		res.Summary = fmt.Sprintf("HTTP %d in %.0f ms", data.Code, data.TotalMS)
	case TypeTraceroute:
		hops := ParseTraceroute(out)
		res.Status = "success"
		res.Data = map[string]interface{}{"hops": hops}
		res.Summary = fmt.Sprintf("%d hop(s)", hops)
	default:
		res.Status = "error"
		res.Error = "unsupported probe type"
		res.Summary = "unsupported probe type"
	}
	return res
}

func commandFor(req Request) (string, error) {
	switch req.Type {
	case TypePing:
		if err := ValidateHost(req.Target); err != nil {
			return "", err
		}
		return BuildPing(req.Target), nil
	case TypeDNS:
		if err := ValidateHost(req.Target); err != nil {
			return "", err
		}
		return BuildDNS(req.Target), nil
	case TypeHTTP:
		if err := ValidateURL(req.Target); err != nil {
			return "", err
		}
		return BuildHTTP(req.Target), nil
	case TypeTraceroute:
		if err := ValidateHost(req.Target); err != nil {
			return "", err
		}
		return BuildTraceroute(req.Target), nil
	default:
		return "", fmt.Errorf("unsupported probe type")
	}
}

// PingResult parses the standard Linux/iputils ping summary.
func ParsePing(out string) (PingData, error) {
	var d PingData
	if m := lossRe.FindStringSubmatch(out); m != nil {
		d.LossPct, _ = strconv.ParseFloat(m[1], 64)
	}
	// "4 packets transmitted, 4 received, 0% packet loss"
	packets := regexp.MustCompile(`([0-9]+) packets transmitted, ([0-9]+) received`).FindStringSubmatch(out)
	if packets != nil {
		d.Transmitted, _ = strconv.Atoi(packets[1])
		d.Received, _ = strconv.Atoi(packets[2])
	}
	if m := rttRe.FindStringSubmatch(out); m != nil {
		d.MinMS, _ = strconv.ParseFloat(m[1], 64)
		d.AvgMS, _ = strconv.ParseFloat(m[2], 64)
		d.MaxMS, _ = strconv.ParseFloat(m[3], 64)
	}
	if d.Transmitted == 0 && d.LossPct == 0 && d.AvgMS == 0 {
		return d, fmt.Errorf("no ping summary in output")
	}
	return d, nil
}

// ParseDNS returns non-empty resolver lines.
func ParseDNS(out string) []string {
	var records []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			records = append(records, line)
		}
	}
	return records
}

// HTTPData is the curl timing payload.
type HTTPData struct {
	Code    int     `json:"code"`
	TotalMS float64 `json:"total_ms"`
	Size    int64   `json:"size"`
}

// ParseHTTP parses the curl -w JSON payload.
func ParseHTTP(out string) (HTTPData, error) {
	out = strings.TrimSpace(out)
	var raw struct {
		Code float64 `json:"code"`
		Time float64 `json:"time"`
		Size int64   `json:"size"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return HTTPData{}, err
	}
	if raw.Code <= 0 {
		return HTTPData{}, fmt.Errorf("no HTTP status code")
	}
	return HTTPData{Code: int(raw.Code), TotalMS: raw.Time * 1000, Size: raw.Size}, nil
}

// ParseTraceroute counts numbered hops in traceroute/mtr output.
func ParseTraceroute(out string) int {
	return len(hopRe.FindAllStringSubmatch(out, -1))
}

// ParseIperf3 extracts the sender/receiver bitrates from `iperf3 -J`.
func ParseIperf3(out string) (IperfData, error) {
	var raw struct {
		End struct {
			SumSent struct {
				BitsPerSecond float64 `json:"bits_per_second"`
			} `json:"sum_sent"`
			SumReceived struct {
				BitsPerSecond float64 `json:"bits_per_second"`
			} `json:"sum_received"`
		} `json:"end"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &raw); err != nil {
		return IperfData{}, err
	}
	sent := raw.End.SumSent.BitsPerSecond
	recv := raw.End.SumReceived.BitsPerSecond
	if sent <= 0 && recv <= 0 {
		return IperfData{}, fmt.Errorf("no iperf3 throughput in output")
	}
	return IperfData{
		SentBPS:     sent,
		ReceivedBPS: recv,
		SentMbps:    sent / 1e6,
		RecvMbps:    recv / 1e6,
	}, nil
}

func capOutput(s string) string {
	if len(s) <= maxOutput {
		return s
	}
	return s[:maxOutput] + "\n… (truncated)"
}
