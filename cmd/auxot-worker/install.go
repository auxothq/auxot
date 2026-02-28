package main

// Daemon management — install, list, uninstall subcommands.
//
// Naming convention:
//   macOS label:  com.auxot.worker.<name>
//   macOS plist:  /Library/LaunchDaemons/com.auxot.worker.<name>.plist  (always-on)
//                 ~/Library/LaunchAgents/com.auxot.worker.<name>.plist   (user-session)
//   Linux unit:   /etc/systemd/system/auxot-worker-<name>.service        (always-on)
//                 ~/.config/systemd/user/auxot-worker-<name>.service     (user-session)

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"text/tabwriter"
)

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ─────────────────────────────────────────────────────────────────────────────
// install
// ─────────────────────────────────────────────────────────────────────────────

func cmdInstall(args []string) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		fmt.Fprintln(os.Stderr, "install is only supported on macOS and Linux")
		os.Exit(1)
	}

	var name, gpuKey, routerURL string
	alwaysOn := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--name":
			if i+1 < len(args) {
				i++
				name = args[i]
			}
		case "--gpu-key":
			if i+1 < len(args) {
				i++
				gpuKey = args[i]
			}
		case "--router-url":
			if i+1 < len(args) {
				i++
				routerURL = args[i]
			}
		case "--always-on":
			alwaysOn = true
		case "--help", "-h":
			printInstallHelp()
			return
		}
	}

	if name == "" {
		name = "default"
	}
	if !validName.MatchString(name) {
		fmt.Fprintf(os.Stderr, "invalid name %q — use lowercase letters, digits, and hyphens only\n", name)
		os.Exit(1)
	}
	if gpuKey == "" {
		fmt.Fprintln(os.Stderr, "error: --gpu-key is required")
		os.Exit(1)
	}
	if routerURL == "" {
		routerURL = "wss://auxot.com/api/gpu/client"
	}

	// Resolve binary path (handles symlinks as installed by Homebrew etc.)
	binaryPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not determine binary path: %v\n", err)
		os.Exit(1)
	}
	binaryPath, err = filepath.EvalSymlinks(binaryPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not resolve binary symlink: %v\n", err)
		os.Exit(1)
	}

	// If not explicitly set, ask whether to install always-on (system daemon) or
	// user-session only.
	if !alwaysOn {
		fmt.Println("Install mode:")
		fmt.Println("  [1] Always-on  — starts on boot, runs as a dedicated system user (recommended)")
		fmt.Println("  [2] User-session — starts when you log in, runs as your user")
		fmt.Print("Choose [1/2]: ")
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Scan()
		choice := strings.TrimSpace(scanner.Text())
		if choice == "1" {
			alwaysOn = true
		}
	}

	switch runtime.GOOS {
	case "darwin":
		installDarwin(name, gpuKey, routerURL, binaryPath, alwaysOn)
	case "linux":
		installLinux(name, gpuKey, routerURL, binaryPath, alwaysOn)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// macOS install
// ─────────────────────────────────────────────────────────────────────────────

func installDarwin(name, gpuKey, routerURL, binaryPath string, alwaysOn bool) {
	label := "com.auxot.worker." + name

	var plistDir string
	if alwaysOn {
		plistDir = "/Library/LaunchDaemons"
	} else {
		home, _ := os.UserHomeDir()
		plistDir = filepath.Join(home, "Library", "LaunchAgents")
	}

	if err := os.MkdirAll(plistDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "cannot create %s: %v\n", plistDir, err)
		os.Exit(1)
	}

	// Ensure auxot system user exists (only for always-on daemons)
	if alwaysOn {
		if err := ensureMacOSUser("auxot"); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not create auxot user: %v\n", err)
			fmt.Fprintln(os.Stderr, "  continuing — daemon will run as root instead")
		}
	}

	logDir := "/var/log"
	if !alwaysOn {
		home, _ := os.UserHomeDir()
		logDir = filepath.Join(home, ".auxot", "logs")
		_ = os.MkdirAll(logDir, 0755)
	}

	plist := buildPlist(label, binaryPath, gpuKey, routerURL, logDir, alwaysOn)
	plistPath := filepath.Join(plistDir, label+".plist")

	if err := os.WriteFile(plistPath, []byte(plist), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "cannot write plist: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\n", plistPath)

	// Load the service
	sysRun("launchctl", "load", "-w", plistPath)

	fmt.Printf("\nauxot-worker %q installed and started.\n", name)
	fmt.Printf("  logs:   %s/com.auxot.worker.%s.log\n", logDir, name)
	fmt.Printf("  status: launchctl list %s\n", label)
	fmt.Printf("  stop:   launchctl unload %s\n", plistPath)
}

func buildPlist(label, binaryPath, gpuKey, routerURL, logDir string, runAsAuxot bool) string {
	var userKey string
	if runAsAuxot {
		userKey = "\n\t<key>UserName</key>\n\t<string>auxot</string>"
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>%s
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>AUXOT_GPU_KEY</key>
		<string>%s</string>
		<key>AUXOT_ROUTER_URL</key>
		<string>%s</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s/%s.log</string>
	<key>StandardErrorPath</key>
	<string>%s/%s.log</string>
</dict>
</plist>
`, label, userKey, binaryPath, gpuKey, routerURL, logDir, label, logDir, label)
}

func ensureMacOSUser(username string) error {
	if _, err := user.Lookup(username); err == nil {
		return nil // already exists
	}

	// Find a free UID in the 300–399 range
	uid := ""
	for i := 300; i < 400; i++ {
		out, _ := runSilent("dscl", ".", "-list", "/Users", "UniqueID")
		if !strings.Contains(out, fmt.Sprintf("%d", i)) {
			uid = fmt.Sprintf("%d", i)
			break
		}
	}
	if uid == "" {
		return fmt.Errorf("could not find a free UID in range 300-399")
	}

	cmds := [][]string{
		{"dscl", ".", "-create", "/Users/" + username},
		{"dscl", ".", "-create", "/Users/" + username, "UniqueID", uid},
		{"dscl", ".", "-create", "/Users/" + username, "PrimaryGroupID", "20"},
		{"dscl", ".", "-create", "/Users/" + username, "UserShell", "/usr/bin/false"},
		{"dscl", ".", "-create", "/Users/" + username, "RealName", "Auxot Worker"},
		{"dscl", ".", "-create", "/Users/" + username, "NFSHomeDirectory", "/var/empty"},
	}
	for _, c := range cmds {
		if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil { //nolint:gosec
			return fmt.Errorf("dscl: %s: %w", string(out), err)
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Linux install
// ─────────────────────────────────────────────────────────────────────────────

func installLinux(name, gpuKey, routerURL, binaryPath string, alwaysOn bool) {
	svcName := "auxot-worker-" + name

	var unitDir string
	if alwaysOn {
		unitDir = "/etc/systemd/system"
	} else {
		home, _ := os.UserHomeDir()
		unitDir = filepath.Join(home, ".config", "systemd", "user")
	}

	if err := os.MkdirAll(unitDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "cannot create %s: %v\n", unitDir, err)
		os.Exit(1)
	}

	if alwaysOn {
		if err := ensureLinuxUser("auxot"); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not create auxot user: %v\n", err)
		}
	}

	unit := buildSystemdUnit(svcName, binaryPath, gpuKey, routerURL, alwaysOn)
	unitPath := filepath.Join(unitDir, svcName+".service")

	if err := os.WriteFile(unitPath, []byte(unit), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "cannot write unit file: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\n", unitPath)

	if alwaysOn {
		sysRun("systemctl", "daemon-reload")
		sysRun("systemctl", "enable", "--now", svcName)
	} else {
		sysRun("systemctl", "--user", "daemon-reload")
		sysRun("systemctl", "--user", "enable", "--now", svcName)
	}

	fmt.Printf("\nauxot-worker %q installed and started.\n", name)
	if alwaysOn {
		fmt.Printf("  logs:   journalctl -u %s -f\n", svcName)
		fmt.Printf("  status: systemctl status %s\n", svcName)
		fmt.Printf("  stop:   systemctl stop %s\n", svcName)
	} else {
		fmt.Printf("  logs:   journalctl --user -u %s -f\n", svcName)
		fmt.Printf("  status: systemctl --user status %s\n", svcName)
		fmt.Printf("  stop:   systemctl --user stop %s\n", svcName)
	}
}

func buildSystemdUnit(svcName, binaryPath, gpuKey, routerURL string, system bool) string {
	user := ""
	if system {
		user = "\nUser=auxot\nGroup=auxot"
	}
	return fmt.Sprintf(`[Unit]
Description=Auxot GPU Worker (%s)
After=network-online.target
Wants=network-online.target

[Service]%s
ExecStart=%s
Environment="AUXOT_GPU_KEY=%s"
Environment="AUXOT_ROUTER_URL=%s"
Restart=always
RestartSec=5

[Install]
WantedBy=%s
`, svcName, user, binaryPath, gpuKey, routerURL, func() string {
		if system {
			return "multi-user.target"
		}
		return "default.target"
	}())
}

func ensureLinuxUser(username string) error {
	if _, err := user.Lookup(username); err == nil {
		return nil
	}
	out, err := exec.Command("useradd", "--system", "--no-create-home", //nolint:gosec
		"--shell", "/usr/sbin/nologin", username).CombinedOutput()
	if err != nil {
		return fmt.Errorf("useradd: %s: %w", string(out), err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// list
// ─────────────────────────────────────────────────────────────────────────────

type workerEntry struct {
	Name      string
	Mode      string // "system" or "user"
	Status    string
	GPUKey    string // masked
	RouterURL string
}

func cmdList() {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		fmt.Fprintln(os.Stderr, "list is only supported on macOS and Linux")
		os.Exit(1)
	}

	entries := findInstalledWorkers()

	if len(entries) == 0 {
		fmt.Println("No auxot workers installed.")
		fmt.Println("Run: auxot-worker install --name <name> --gpu-key <key>")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tMODE\tSTATUS\tGPU KEY\tROUTER URL")
	for _, e := range entries {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", e.Name, e.Mode, e.Status, e.GPUKey, e.RouterURL)
	}
	w.Flush()
}

func findInstalledWorkers() []workerEntry {
	var entries []workerEntry

	switch runtime.GOOS {
	case "darwin":
		// System daemons
		entries = append(entries, scanDarwinDir("/Library/LaunchDaemons", "system")...)
		// User agents
		if home, err := os.UserHomeDir(); err == nil {
			entries = append(entries, scanDarwinDir(filepath.Join(home, "Library", "LaunchAgents"), "user")...)
		}
	case "linux":
		// System units
		entries = append(entries, scanLinuxDir("/etc/systemd/system", "system")...)
		// User units
		if home, err := os.UserHomeDir(); err == nil {
			entries = append(entries, scanLinuxDir(filepath.Join(home, ".config", "systemd", "user"), "user")...)
		}
	}

	return entries
}

// scanDarwinDir looks for plist files matching com.auxot.worker.<name>.plist
func scanDarwinDir(dir, mode string) []workerEntry {
	matches, err := filepath.Glob(filepath.Join(dir, "com.auxot.worker.*.plist"))
	if err != nil || len(matches) == 0 {
		return nil
	}
	var out []workerEntry
	for _, path := range matches {
		base := filepath.Base(path)
		// com.auxot.worker.<name>.plist
		name := strings.TrimPrefix(base, "com.auxot.worker.")
		name = strings.TrimSuffix(name, ".plist")

		gpuKey, routerURL := parsePlistEnv(path)
		label := "com.auxot.worker." + name
		status := darwinServiceStatus(label)

		out = append(out, workerEntry{
			Name:      name,
			Mode:      mode,
			Status:    status,
			GPUKey:    maskKey(gpuKey),
			RouterURL: routerURL,
		})
	}
	return out
}

// parsePlistEnv extracts AUXOT_GPU_KEY and AUXOT_ROUTER_URL from the plist XML.
// We wrote it, so the format is known and a simple string scan is sufficient.
func parsePlistEnv(path string) (gpuKey, routerURL string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	gpuKey = plistStringAfterKey(data, "AUXOT_GPU_KEY")
	routerURL = plistStringAfterKey(data, "AUXOT_ROUTER_URL")
	return
}

// plistStringAfterKey extracts the <string> value that follows the <string>keyName</string> in a plist.
var plistStringRe = regexp.MustCompile(`<string>([^<]*)</string>`)

func plistStringAfterKey(data []byte, key string) string {
	marker := []byte("<string>" + key + "</string>")
	idx := bytes.Index(data, marker)
	if idx < 0 {
		return ""
	}
	rest := data[idx+len(marker):]
	m := plistStringRe.FindSubmatch(rest)
	if m == nil {
		return ""
	}
	return string(m[1])
}

func darwinServiceStatus(label string) string {
	out, err := exec.Command("launchctl", "list", label).Output() //nolint:gosec
	if err != nil {
		return "stopped"
	}
	// launchctl list output has a PID column; if the second field is non-zero, it's running.
	// Format: PID  Status  Label
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[2] == label {
			if fields[0] != "-" && fields[0] != "0" {
				return "running"
			}
		}
	}
	return "stopped"
}

// scanLinuxDir looks for unit files matching auxot-worker-<name>.service
func scanLinuxDir(dir, mode string) []workerEntry {
	matches, err := filepath.Glob(filepath.Join(dir, "auxot-worker-*.service"))
	if err != nil || len(matches) == 0 {
		return nil
	}
	var out []workerEntry
	for _, path := range matches {
		base := filepath.Base(path)
		name := strings.TrimPrefix(base, "auxot-worker-")
		name = strings.TrimSuffix(name, ".service")

		gpuKey, routerURL := parseSystemdEnv(path)
		svcName := "auxot-worker-" + name
		status := linuxServiceStatus(svcName, mode == "user")

		out = append(out, workerEntry{
			Name:      name,
			Mode:      mode,
			Status:    status,
			GPUKey:    maskKey(gpuKey),
			RouterURL: routerURL,
		})
	}
	return out
}

// parseSystemdEnv reads Environment= lines from a systemd unit file.
func parseSystemdEnv(path string) (gpuKey, routerURL string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, `Environment="AUXOT_GPU_KEY=`); ok {
			gpuKey = strings.TrimSuffix(after, `"`)
		}
		if after, ok := strings.CutPrefix(line, `Environment="AUXOT_ROUTER_URL=`); ok {
			routerURL = strings.TrimSuffix(after, `"`)
		}
	}
	return
}

func linuxServiceStatus(svcName string, userUnit bool) string {
	args := []string{"is-active", svcName}
	if userUnit {
		args = append([]string{"--user"}, args...)
	}
	out, err := exec.Command("systemctl", args...).Output() //nolint:gosec
	if err != nil {
		return "stopped"
	}
	if strings.TrimSpace(string(out)) == "active" {
		return "running"
	}
	return "stopped"
}

// maskKey shows the first 8 and last 4 characters of a key.
func maskKey(key string) string {
	if key == "" {
		return "(none)"
	}
	if len(key) <= 12 {
		return key[:4] + "..."
	}
	return key[:8] + "..." + key[len(key)-4:]
}

// ─────────────────────────────────────────────────────────────────────────────
// uninstall
// ─────────────────────────────────────────────────────────────────────────────

func cmdUninstall(args []string) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		fmt.Fprintln(os.Stderr, "uninstall is only supported on macOS and Linux")
		os.Exit(1)
	}

	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Println("Usage: auxot-worker uninstall <name>")
		fmt.Println()
		fmt.Println("Remove a named worker install. Run 'auxot-worker list' to see installed workers.")
		return
	}

	name := args[0]

	switch runtime.GOOS {
	case "darwin":
		uninstallDarwin(name)
	case "linux":
		uninstallLinux(name)
	}
}

func uninstallDarwin(name string) {
	label := "com.auxot.worker." + name
	removed := false

	// Check system daemons first, then user agents
	for _, dir := range []string{"/Library/LaunchDaemons"} {
		path := filepath.Join(dir, label+".plist")
		if _, err := os.Stat(path); err == nil {
			runQuiet("launchctl", "unload", "-w", path)
			if err := os.Remove(path); err != nil {
				fmt.Fprintf(os.Stderr, "could not remove %s: %v\n", path, err)
			} else {
				fmt.Printf("removed %s\n", path)
				removed = true
			}
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		path := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
		if _, err := os.Stat(path); err == nil {
			runQuiet("launchctl", "unload", "-w", path)
			if err := os.Remove(path); err != nil {
				fmt.Fprintf(os.Stderr, "could not remove %s: %v\n", path, err)
			} else {
				fmt.Printf("removed %s\n", path)
				removed = true
			}
		}
	}

	if !removed {
		fmt.Fprintf(os.Stderr, "no worker named %q found — run 'auxot-worker list'\n", name)
		os.Exit(1)
	}
	fmt.Printf("worker %q uninstalled\n", name)
}

func uninstallLinux(name string) {
	svcName := "auxot-worker-" + name
	removed := false

	if path := "/etc/systemd/system/" + svcName + ".service"; fileExists(path) {
		runQuiet("systemctl", "disable", "--now", svcName)
		if err := os.Remove(path); err != nil {
			fmt.Fprintf(os.Stderr, "could not remove %s: %v\n", path, err)
		} else {
			fmt.Printf("removed %s\n", path)
			removed = true
		}
		sysRun("systemctl", "daemon-reload")
	}

	if home, err := os.UserHomeDir(); err == nil {
		path := filepath.Join(home, ".config", "systemd", "user", svcName+".service")
		if fileExists(path) {
			runQuiet("systemctl", "--user", "disable", "--now", svcName)
			if err := os.Remove(path); err != nil {
				fmt.Fprintf(os.Stderr, "could not remove %s: %v\n", path, err)
			} else {
				fmt.Printf("removed %s\n", path)
				removed = true
			}
			sysRun("systemctl", "--user", "daemon-reload")
		}
	}

	if !removed {
		fmt.Fprintf(os.Stderr, "no worker named %q found — run 'auxot-worker list'\n", name)
		os.Exit(1)
	}
	fmt.Printf("worker %q uninstalled\n", name)
}

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

func sysRun(name string, args ...string) {
	cmd := exec.Command(name, args...) //nolint:gosec
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s %v: %v\n", name, args, err)
	}
}

func runQuiet(name string, args ...string) {
	_ = exec.Command(name, args...).Run() //nolint:gosec
}

func runSilent(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output() //nolint:gosec
	return string(out), err
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func printInstallHelp() {
	fmt.Print(`Usage: auxot-worker install [flags]

Install auxot-worker as a persistent daemon (runs on boot).

Flags:
  --name <name>        Worker name, used to identify this install (default: "default")
                       Allows multiple workers with different GPU keys/models.
  --gpu-key <key>      GPU key for this worker (required)
  --router-url <url>   Router WebSocket URL (default: wss://auxot.com/api/gpu/client)
  --always-on          Install as system daemon (no interactive prompt)

Examples:
  auxot-worker install --name qwen --gpu-key gpu_abc123
  auxot-worker install --name llama --gpu-key gpu_xyz789 --always-on
  auxot-worker install --name flux  --gpu-key gpu_def456 --router-url wss://custom.example.com/ws
`)
}
