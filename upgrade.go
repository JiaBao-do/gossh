package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
)

// releaseRepo is where releases (and the install scripts) are published.
const releaseRepo = "JiaBao-do/gossh"

var httpClient = &http.Client{Timeout: 5 * time.Minute}

// parseVersion reads a release tag like v1.2.3. Development builds
// ("dev", "v1.2.3-4-gabcdef-dirty") are not releases.
func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// upgradeNeeded says whether latest should replace the running version,
// and if not, why.
func upgradeNeeded(current, latest string) (bool, string) {
	cur, ok := parseVersion(current)
	if !ok {
		return true, "" // development build: any release replaces it
	}
	lat, ok := parseVersion(latest)
	if !ok {
		return false, fmt.Sprintf("latest release has an unexpected tag %q", latest)
	}
	for i := range cur {
		if cur[i] != lat[i] {
			if cur[i] > lat[i] {
				return false, fmt.Sprintf("gossh %s is newer than the latest release %s", current, latest)
			}
			return true, ""
		}
	}
	return false, fmt.Sprintf("gossh %s is already the latest version", current)
}

func fetch(url string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "gossh/"+version)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func latestRelease() (string, error) {
	body, err := fetch("https://api.github.com/repos/" + releaseRepo + "/releases/latest")
	if err != nil {
		return "", fmt.Errorf("checking for a new release: %w", err)
	}
	var r struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.TagName == "" {
		return "", fmt.Errorf("checking for a new release: unexpected reply from GitHub")
	}
	return r.TagName, nil
}

// verifyChecksum checks data against asset's line in a sha256sum-style file.
func verifyChecksum(sums []byte, asset string, data []byte) error {
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != asset {
			continue
		}
		sum := sha256.Sum256(data)
		if !strings.EqualFold(fields[0], hex.EncodeToString(sum[:])) {
			return fmt.Errorf("checksum mismatch for %s; not installing it", asset)
		}
		return nil
	}
	return fmt.Errorf("no checksum listed for %s", asset)
}

// installRelease downloads tag for this OS/CPU, verifies it and replaces
// the running executable.
func installRelease(tag string) error {
	asset := fmt.Sprintf("gossh-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		asset += ".exe"
	}
	base := "https://github.com/" + releaseRepo + "/releases/download/" + tag + "/"
	bin, err := fetch(base + asset)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", asset, err)
	}
	sums, err := fetch(base + "checksums.txt")
	if err != nil {
		return fmt.Errorf("downloading checksums: %w", err)
	}
	if err := verifyChecksum(sums, asset, bin); err != nil {
		return err
	}
	if !bytes.HasPrefix(bin, []byte("MZ")) && !bytes.HasPrefix(bin, []byte("\x7fELF")) && runtime.GOOS != "darwin" {
		return fmt.Errorf("downloaded %s is not a program", asset)
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	return replaceExecutable(exe, bin)
}

// replaceExecutable swaps exe for data. Windows can't overwrite a running
// .exe but can rename it, so the old one is moved aside and removed on the
// next start (see cleanupOldBinary).
func replaceExecutable(exe string, data []byte) error {
	tmp := exe + ".new"
	if err := os.WriteFile(tmp, data, 0755); err != nil {
		return fmt.Errorf("writing next to %s (is that folder writable? if gossh was installed with sudo, rerun the installer): %w", exe, err)
	}
	if runtime.GOOS == "windows" {
		old := exe + ".old"
		os.Remove(old)
		if err := os.Rename(exe, old); err != nil {
			os.Remove(tmp)
			return err
		}
		if err := os.Rename(tmp, exe); err != nil {
			os.Rename(old, exe)
			return err
		}
		return nil
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// cleanupOldBinary removes the executable left behind by a Windows upgrade.
func cleanupOldBinary() {
	if runtime.GOOS != "windows" {
		return
	}
	if exe, err := os.Executable(); err == nil {
		os.Remove(exe + ".old")
	}
}

// upgradeCLI implements "gossh upgrade".
func upgradeCLI() error {
	fmt.Println("Checking for a new gossh release...")
	tag, err := latestRelease()
	if err != nil {
		return err
	}
	if needed, why := upgradeNeeded(version, tag); !needed {
		fmt.Println(why)
		return nil
	}
	fmt.Printf("Downloading gossh %s...\n", tag)
	if err := installRelease(tag); err != nil {
		return err
	}
	fmt.Printf("Upgraded gossh %s -> %s\n", version, tag)
	return nil
}

// upgradeInteractive is the "u" action in the host list.
func upgradeInteractive() (upgraded bool, msg string, err error) {
	fmt.Println("Checking for a new gossh release...")
	tag, err := latestRelease()
	if err != nil {
		return false, "", err
	}
	if needed, why := upgradeNeeded(version, tag); !needed {
		return false, why, nil
	}
	ok := true
	if err := huh.NewConfirm().
		Title(fmt.Sprintf("Upgrade gossh %s to %s?", version, tag)).
		Affirmative("Upgrade").
		Negative("Not now").
		Value(&ok).
		Run(); err != nil || !ok {
		return false, "", err
	}
	fmt.Printf("Downloading gossh %s...\n", tag)
	if err := installRelease(tag); err != nil {
		return false, "", err
	}
	return true, fmt.Sprintf("Upgraded gossh %s -> %s. Run gossh again to use the new version.", version, tag), nil
}
