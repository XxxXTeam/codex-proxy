// Package browser provides cross-platform functionality for opening URLs in the default web browser.
package browser

import (
	"fmt"
	"os/exec"
	"runtime"
)

// OpenURL opens the specified URL in the default web browser.
func OpenURL(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		// Try rundll32 first, fallback to cmd /c start
		if err := exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start(); err == nil {
			return nil
		}
		return exec.Command("cmd", "/c", "start", "", url).Start()
	case "linux":
		browsers := []string{"xdg-open", "x-www-browser", "www-browser", "firefox", "chromium", "google-chrome"}
		for _, browser := range browsers {
			if path, err := exec.LookPath(browser); err == nil {
				return exec.Command(path, url).Start()
			}
		}
		return fmt.Errorf("no suitable browser found on Linux system")
	default:
		return fmt.Errorf("unsupported operating system: %s", runtime.GOOS)
	}
}

// IsAvailable checks if the system has a command available to open a web browser.
func IsAvailable() bool {
	switch runtime.GOOS {
	case "darwin":
		_, err := exec.LookPath("open")
		return err == nil
	case "windows":
		_, err := exec.LookPath("rundll32")
		return err == nil
	case "linux":
		browsers := []string{"xdg-open", "x-www-browser", "www-browser", "firefox", "chromium", "google-chrome"}
		for _, browser := range browsers {
			if _, err := exec.LookPath(browser); err == nil {
				return true
			}
		}
		return false
	default:
		return false
	}
}
