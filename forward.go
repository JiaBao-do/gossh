package main

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
)

// forward is one parsed port-forward spec.
type forward struct {
	flag  string // "-L", "-R" or "-D"
	value string // argument passed to ssh
	desc  string // human readable summary
	local string // local port to check before starting ("" for -R)
}

func validPort(p string) error {
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("invalid port %q", p)
	}
	return nil
}

// parseForwardSpec understands a short syntax:
//
//	8080                 local 8080   -> remote localhost:8080
//	8080:80              local 8080   -> remote localhost:80
//	8080:db:5432         local 8080   -> db:5432 as seen from the server
//	R:9000:3000          server :9000 -> your localhost:3000
//	R:9000:web:3000      server :9000 -> web:3000 as seen from your machine
//	D:1080               SOCKS proxy on local 1080
//
// The L:, R: and D: prefixes are case-insensitive; L is the default. A full
// ssh spec with a bind address (4 parts for L/R, 2 for D) is passed through.
func parseForwardSpec(spec, via string) (forward, error) {
	kind := "L"
	if len(spec) > 2 && spec[1] == ':' && strings.ContainsAny(spec[:1], "LRDlrd") {
		kind, spec = strings.ToUpper(spec[:1]), spec[2:]
	}
	parts := strings.Split(spec, ":")
	bad := fmt.Errorf("invalid forward %q (try 8080, 8080:80, 8080:host:80, R:9000:3000 or D:1080)", spec)

	switch kind {
	case "L", "R":
		var bind, first, host, second string
		switch len(parts) {
		case 1:
			first, host, second = parts[0], "localhost", parts[0]
		case 2:
			first, host, second = parts[0], "localhost", parts[1]
		case 3:
			first, host, second = parts[0], parts[1], parts[2]
		case 4:
			bind, first, host, second = parts[0], parts[1], parts[2], parts[3]
		default:
			return forward{}, bad
		}
		if host == "" {
			return forward{}, bad
		}
		for _, p := range []string{first, second} {
			if err := validPort(p); err != nil {
				return forward{}, err
			}
		}
		value := first + ":" + host + ":" + second
		if bind != "" {
			value = bind + ":" + value
		}
		if kind == "L" {
			return forward{
				flag:  "-L",
				value: value,
				desc:  fmt.Sprintf("localhost:%s  ->  %s:%s (via %s)", first, host, second, via),
				local: first,
			}, nil
		}
		return forward{
			flag:  "-R",
			value: value,
			desc:  fmt.Sprintf("%s:%s  ->  %s:%s on this machine", via, first, host, second),
		}, nil

	case "D":
		port := parts[len(parts)-1]
		if len(parts) > 2 {
			return forward{}, bad
		}
		if err := validPort(port); err != nil {
			return forward{}, err
		}
		return forward{
			flag:  "-D",
			value: spec,
			desc:  fmt.Sprintf("SOCKS5 proxy on localhost:%s (traffic exits from %s)", port, via),
			local: port,
		}, nil
	}
	return forward{}, fmt.Errorf("unknown forward type %q (use L, R or D)", kind)
}

// portFree reports whether a local TCP port can be bound.
func portFree(port string) bool {
	l, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// startForwards validates specs and runs ssh until the user presses Ctrl+C.
func startForwards(host string, specs []string) (string, error) {
	var fwds []forward
	for _, s := range specs {
		f, err := parseForwardSpec(s, host)
		if err != nil {
			return "", err
		}
		if f.local != "" && !portFree(f.local) {
			return "", fmt.Errorf("local port %s is already in use; pick another one", f.local)
		}
		fwds = append(fwds, f)
	}

	sshArgs := []string{"-N", "-o", "ExitOnForwardFailure=yes", "-o", "ServerAliveInterval=30"}
	fmt.Printf("Forwarding through %s:\n", host)
	for _, f := range fwds {
		fmt.Println("  " + f.desc)
		sshArgs = append(sshArgs, f.flag, f.value)
	}
	sshArgs = append(sshArgs, host)
	fmt.Println("Press Ctrl+C to stop and go back to gossh.")
	if code := runSSH(sshArgs...); code == 255 {
		waitForEnter()
		return "", fmt.Errorf("forwarding through %s failed (see the ssh message)", host)
	}
	return "Stopped forwarding through " + host, nil
}

// forwardInteractive guides the user through a single forward.
func forwardInteractive(host string) (string, error) {
	kind := "L"
	if err := huh.NewSelect[string]().
		Title("What do you want to do?").
		Options(
			huh.NewOption("Access a remote service on this machine   (local  -L)", "L"),
			huh.NewOption("Expose a service on this machine remotely (remote -R)", "R"),
			huh.NewOption("SOCKS proxy through the server            (dynamic -D)", "D"),
		).
		Value(&kind).
		Run(); err != nil {
		return "", err
	}

	var spec string
	switch kind {
	case "L":
		target, local := "localhost:80", ""
		if err := huh.NewForm(huh.NewGroup(
			huh.NewInput().
				Title("Remote service (host:port, as seen from "+host+")").
				Description("e.g. localhost:5432 or db.internal:3306").
				Value(&target).
				Validate(validateTarget),
			huh.NewInput().
				Title("Local port").
				Description("Leave empty to use the same port").
				Value(&local).
				Validate(optionalPort),
		)).Run(); err != nil {
			return "", err
		}
		h, p := splitTarget(target)
		if local == "" {
			local = p
		}
		spec = local + ":" + h + ":" + p

	case "R":
		remote, target := "", "localhost:3000"
		if err := huh.NewForm(huh.NewGroup(
			huh.NewInput().
				Title("Local service (host:port on this machine)").
				Value(&target).
				Validate(validateTarget),
			huh.NewInput().
				Title("Port to open on "+host).
				Description("Leave empty to use the same port").
				Value(&remote).
				Validate(optionalPort),
		)).Run(); err != nil {
			return "", err
		}
		h, p := splitTarget(target)
		if remote == "" {
			remote = p
		}
		spec = "R:" + remote + ":" + h + ":" + p

	case "D":
		port := "1080"
		if err := huh.NewInput().
			Title("Local SOCKS port").
			Value(&port).
			Validate(validPort).
			Run(); err != nil {
			return "", err
		}
		spec = "D:" + port
	}

	return startForwards(host, []string{spec})
}

// splitTarget accepts "host:port" or a bare port (meaning localhost).
func splitTarget(s string) (host, port string) {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, ":"); i >= 0 {
		return s[:i], s[i+1:]
	}
	return "localhost", s
}

func validateTarget(s string) error {
	h, p := splitTarget(s)
	if h == "" {
		return fmt.Errorf("host is required")
	}
	return validPort(p)
}

func optionalPort(s string) error {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return validPort(s)
}
