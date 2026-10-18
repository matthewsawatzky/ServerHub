package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"serverhub/internal/api"
	"serverhub/internal/auth"
	"serverhub/internal/config"
	"serverhub/internal/debug"
	"serverhub/internal/local"
	sshclient "serverhub/internal/ssh"
	"serverhub/internal/storage"
)

//go:embed web/*
var webFS embed.FS

const version = "1.0.0"

// ── ANSI helpers ──────────────────────────────────────────────────────────────

func ansi(code, s string) string  { return "\033[" + code + "m" + s + "\033[0m" }
func bold(s string) string        { return ansi("1", s) }
func dim(s string) string         { return ansi("2", s) }
func green(s string) string       { return ansi("38;5;64", s) }
func brightGreen(s string) string { return ansi("1;38;5;64", s) }
func red(s string) string         { return ansi("31", s) }
func yellow(s string) string      { return ansi("33", s) }

// ── Banner ────────────────────────────────────────────────────────────────────

var artLines = []string{
	`███████╗███████╗██████╗ ██╗   ██╗███████╗██████╗ ██╗  ██╗██╗   ██╗██████╗ `,
	`██╔════╝██╔════╝██╔══██╗██║   ██║██╔════╝██╔══██╗██║  ██║██║   ██║██╔══██╗`,
	`███████╗█████╗  ██████╔╝██║   ██║█████╗  ██████╔╝███████║██║   ██║██████╔╝`,
	`╚════██║██╔══╝  ██╔══██╗╚██╗ ██╔╝██╔══╝  ██╔══██╗██╔══██║██║   ██║██╔══██╗`,
	`███████║███████╗██║  ██║ ╚████╔╝ ███████╗██║  ██║██║  ██║╚██████╔╝██████╔╝`,
	`╚══════╝╚══════╝╚═╝  ╚═╝  ╚═══╝  ╚══════╝╚═╝  ╚═╝╚═╝  ╚═╝ ╚═════╝ ╚═════╝`,
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

func artWidth() int {
	max := 0
	for _, l := range artLines {
		if n := runeLen(l); n > max {
			max = n
		}
	}
	return max
}

func centre(s string, width int) string {
	n := runeLen(s)
	if n >= width {
		return s
	}
	pad := (width - n) / 2
	return strings.Repeat(" ", pad) + s
}

func hline(width int) string {
	return strings.Repeat("─", width)
}

func printReady(url string, serverCount int) {
	fmt.Println()
	fmt.Printf("  %s  %s\n", brightGreen("●"), bold(url))
	fmt.Println()
	switch serverCount {
	case 0:
		fmt.Println(dim("     No servers configured — add them from the dashboard"))
	case 1:
		fmt.Println(dim("     1 server"))
	default:
		fmt.Printf(dim("     %d servers\n"), serverCount)
	}
	fmt.Println()
	fmt.Println(dim("     Press " + bold("q") + dim(" to quit")))
	fmt.Println()
}

func termWidth() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return 80
	}
	return w
}

func printBanner() {
	fmt.Print("\033[H\033[2J\033[3J")

	w := termWidth()
	art := artWidth()

	fmt.Println()

	if w >= art+4 {
		for _, l := range artLines {
			fmt.Println(brightGreen(centre(l, w)))
		}
		subtitle := fmt.Sprintf("Personal Server Dashboard  v%s", version)
		fmt.Println()
		fmt.Println(dim(centre(subtitle, w)))
		fmt.Println()
		fmt.Println(dim(hline(w)))
	} else {
		fmt.Println(bold("  ServerHub") + dim(fmt.Sprintf("  v%s", version)))
		fmt.Println(dim("  Personal Server Dashboard"))
		fmt.Println()
		fmt.Println(dim(fmt.Sprintf("  (terminal too narrow for the full banner — widen it, you're missing out)")))
		fmt.Println()
		fmt.Println(dim(hline(w)))
	}

	fmt.Println()
}

// ── Port conflict handling ────────────────────────────────────────────────────

func isAddrInUse(err error) bool {
	return strings.Contains(err.Error(), "address already in use") ||
		strings.Contains(err.Error(), "Only one usage of each socket address")
}

// handlePortConflict probes the running service on the same port.
// If it's another ServerHub instance, opens the browser and closes this window.
// Otherwise it prints an error and exits.
func handlePortConflict(port int, bind string) {
	host := localHost(bind)
	url := fmt.Sprintf("http://%s:%d", host, port)

	fmt.Printf("\r\n  %s Port %d is already in use — checking...\r\n", yellow("!"), port)
	time.Sleep(300 * time.Millisecond)

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url + "/api/ping")
	if err != nil {
		fmt.Fprintf(os.Stderr, "\r\n  %s Port %d is in use by another application — cannot start.\r\n\r\n", red("✗"), port)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var payload struct {
		App string `json:"app"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil || payload.App != "serverhub" {
		fmt.Fprintf(os.Stderr, "\r\n  %s Port %d is in use by another application — cannot start.\r\n\r\n", red("✗"), port)
		os.Exit(1)
	}

	// It's another ServerHub — explain, open browser, then close this window.
	w := termWidth()
	fmt.Println(dim(hline(w)))
	fmt.Println()
	fmt.Printf("  %s  Another instance of ServerHub is already running.\r\n", brightGreen("●"))
	fmt.Println()
	fmt.Printf("       %s\r\n", bold(url))
	fmt.Println()
	fmt.Println(dim("  Opening the existing instance in your browser."))
	fmt.Println(dim("  This window will close automatically."))
	fmt.Println()
	fmt.Println(dim(hline(w)))
	fmt.Println()

	// Open browser first, wait long enough for the OS to act on it.
	openBrowser(url)
	time.Sleep(800 * time.Millisecond)

	closeTerminalWindow()
	os.Exit(0)
}

// closeTerminalWindow attempts to close the front terminal window on macOS.
// On other platforms it is a no-op; os.Exit(0) in the caller will do the job.
func closeTerminalWindow() {
	if runtime.GOOS != "darwin" {
		return
	}
	script := `
tell application "System Events"
	set frontApp to name of (first application process whose frontmost is true)
end tell
if frontApp is "Terminal" then
	tell application "Terminal"
		if (count of windows) > 0 then close front window
	end tell
else if frontApp is "iTerm2" then
	tell application "iTerm2"
		tell current window
			tell current session to close
		end tell
	end tell
end if`
	exec.Command("osascript", "-e", script).Start() //nolint:errcheck — best-effort
	time.Sleep(300 * time.Millisecond)
}

// ── Keyboard quit listener ────────────────────────────────────────────────────

// listenForQuit reads raw keypresses from stdin. Pressing 'q' shows a
// confirmation prompt; 'y' shuts the server down gracefully.
func listenForQuit(srv *http.Server) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return
	}

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return
	}

	restore := func() { term.Restore(int(os.Stdin.Fd()), oldState) } //nolint:errcheck

	buf := make([]byte, 1)
	for {
		if _, err := os.Stdin.Read(buf); err != nil {
			restore()
			return
		}

		if buf[0] != 'q' && buf[0] != 'Q' {
			continue
		}

		// Restore normal terminal for the prompt line.
		restore()
		fmt.Print("\r\n  Quit ServerHub? [y/N]: ")

		// One more raw read for the single-key answer.
		rawState, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			// Fall back to a normal read.
			var answer string
			fmt.Scanln(&answer)
			fmt.Print("\r\n")
			if strings.ToLower(strings.TrimSpace(answer)) == "y" {
				doShutdown(srv)
				return
			}
		} else {
			os.Stdin.Read(buf) //nolint:errcheck
			term.Restore(int(os.Stdin.Fd()), rawState)
			fmt.Print("\r\n")

			if buf[0] == 'y' || buf[0] == 'Y' {
				doShutdown(srv)
				return
			}
		}

		// Not confirmed — resume raw listening.
		oldState, err = term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			return
		}
	}
}

func doShutdown(srv *http.Server) {
	fmt.Print("\r\n  " + dim("Shutting down...") + "\r\n\r\n")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	srv.Shutdown(ctx) //nolint:errcheck
	os.Exit(0)
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	log.SetOutput(debug.NewLogInterceptor(io.Discard))

	printBanner()

	var cfgPath string
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	} else {
		path, _, err := config.EnsureDefault()
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
		cfgPath = path
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR loading config: %v\n", err)
		os.Exit(1)
	}

	entries := make([]*sshclient.ServerEntry, len(cfg.Servers))
	for i, s := range cfg.Servers {
		entries[i] = sshclient.NewServerEntry(s.ID, s.Host, s.Port, s.User, s.Key)
	}

	// SSH host key policy goes live before anything dials (audit V6).
	sshclient.SetHostKeyPolicy(cfg.Server.InsecureIgnoreHostKey, knownHostsPath())

	// Auth state lives next to config.yaml (auth.json + sessions.json).
	authDir, err := storage.Dir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	authMgr, err := auth.New(authDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: unusable auth state: %v\n", err)
		os.Exit(1)
	}
	authMgr.Audit = api.LogAction

	sshclient.StartPolling(entries)

	lc := local.New()
	lc.Start()

	mux := http.NewServeMux()
	hub := api.NewHub(cfg, cfgPath, entries, lc)
	authMgr.Theme = hub.ThemeValues // login page pre-applies the instance look
	hub.SetAuthManager(authMgr)
	hub.RegisterRoutes(mux)
	auth.RegisterRoutes(mux, authMgr)

	// Onboarding explainer image — optional asset the owner can drop into
	// go/web/assets/ (see README). It is public while the dashboard is open
	// (no password yet, or opt-out mode); once password auth is active it
	// requires a session like everything else.
	mux.HandleFunc("/onboarding-image", func(w http.ResponseWriter, r *http.Request) {
		if authMgr.AuthRequired() && !authMgr.AuthenticateRequest(r) {
			http.NotFound(w, r)
			return
		}
		for _, name := range []string{
			"web/assets/onboarding-image.png",
			"web/assets/onboarding-image.jpg",
			"web/assets/onboarding-image.jpeg",
			"web/assets/onboarding-image.webp",
			"web/assets/onboarding-image.gif",
			"web/assets/onboarding-image.svg",
		} {
			data, err := webFS.ReadFile(name)
			if err != nil {
				continue
			}
			switch ext := strings.TrimPrefix(filepath.Ext(name), "."); ext {
			case "png":
				w.Header().Set("Content-Type", "image/png")
			case "jpg", "jpeg":
				w.Header().Set("Content-Type", "image/jpeg")
			case "webp":
				w.Header().Set("Content-Type", "image/webp")
			case "gif":
				w.Header().Set("Content-Type", "image/gif")
			case "svg":
				w.Header().Set("Content-Type", "image/svg+xml")
			}
			w.Header().Set("Cache-Control", "no-store")
			w.Write(data)
			return
		}
		http.NotFound(w, r)
	})

	// Login page — self-contained (no external assets), always publicly reachable.
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		data, err := webFS.ReadFile("web/login.html")
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(data)
	})

	mux.HandleFunc("/terminal/", func(w http.ResponseWriter, r *http.Request) {
		data, err := webFS.ReadFile("web/terminal.html")
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})

	mux.HandleFunc("/debug", func(w http.ResponseWriter, r *http.Request) {
		data, err := webFS.ReadFile("web/debug.html")
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})

	webRoot, err := fs.Sub(webFS, "web")
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	mux.Handle("/", http.FileServer(http.FS(webRoot)))

	addr := fmt.Sprintf("%s:%d", cfg.Server.Bind, cfg.Server.WebPort)
	url := fmt.Sprintf("http://%s:%d", localHost(cfg.Server.Bind), cfg.Server.WebPort)

	// Try to claim the port before doing anything else.
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		if isAddrInUse(err) {
			handlePortConflict(cfg.Server.WebPort, cfg.Server.Bind)
		}
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	printReady(url, len(cfg.Servers))

	// Loud warning when the dashboard leaves the loopback (audit V13):
	// everything travels as plaintext HTTP/WS — network-level encryption is
	// then the user's responsibility (e.g. inside a Tailscale/WireGuard overlay).
	if !isLoopbackBind(cfg.Server.Bind) {
		w := termWidth()
		fmt.Println(yellow(hline(w)))
		fmt.Println()
		fmt.Printf("  %s  %s\n", yellow("!"), bold(fmt.Sprintf("ServerHub is binding to a non-loopback address (%s)", cfg.Server.Bind)))
		fmt.Println()
		fmt.Println(dim("  All traffic is plaintext HTTP (passwords typed into terminals included)."))
		fmt.Println(dim("  Put the connection in an encrypted overlay (Tailscale/WireGuard/SSH tunnel)."))
		if !authMgr.AuthRequired() {
			fmt.Println(yellow("  This instance has NO dashboard password — anyone who can reach this"))
			fmt.Println(yellow("  address can control every server it manages."))
		}
		fmt.Println(dim("  Never bind 0.0.0.0 without additional access controls."))
		fmt.Println()
		fmt.Println(yellow(hline(w)))
		log.Printf("ServerHub bound to non-loopback address %s — ensure network-level auth and encryption", cfg.Server.Bind)
	}

	if cfg.Server.OpenBrowser {
		go func() {
			time.Sleep(500 * time.Millisecond)
			openBrowser(url)
		}()
	}

	// Middleware chain (outermost → innermost):
	//   guard (Host allowlist + origin/preflight checks, audit V4)
	//   → auth (session cookie; WS upgrades included, audit V1/V2)
	//   → debug timing wrapper (captures authenticated requests only)
	//   → mux
	handler := api.NewGuard(cfg.Server.Bind, cfg.Server.WebPort).Middleware(
		auth.Middleware(authMgr, debug.Middleware(mux)),
	)

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second, // no global WriteTimeout — it would kill SSE/WS
		IdleTimeout:       120 * time.Second,
	}
	go listenForQuit(srv)

	if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
}

// knownHostsPath is where SSH host keys are pinned (TOFU, audit V6).
func knownHostsPath() string {
	dir, err := storage.Dir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "known_hosts")
}

// isLoopbackBind reports whether the configured bind address keeps the
// dashboard on the local machine only.
func isLoopbackBind(bind string) bool {
	if bind == "" || bind == "localhost" {
		return true
	}
	ip := net.ParseIP(bind)
	return ip != nil && ip.IsLoopback()
}

func localHost(bind string) string {
	if bind == "0.0.0.0" || bind == "" {
		return "localhost"
	}
	return bind
}

func openBrowser(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd, args = "open", []string{url}
	case "linux":
		cmd, args = "xdg-open", []string{url}
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	default:
		return
	}
	exec.Command(cmd, args...).Start() //nolint:errcheck — best-effort
}
