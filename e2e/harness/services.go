//go:build e2e

package harness

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	networkName    = "eru-e2e"
	minioName      = "eru-e2e-minio"
	pebbleName     = "eru-e2e-pebble"
	challName      = "eru-e2e-challtestsrv"
	minioPort      = "19000"
	pebblePort     = "14000"
	pebbleMgmtPort = "15000"
	challMgmtPort  = "8055"
	challDNSPort   = "8053"
)

// Env describes the object store and ACME services shared by a test run.
type Env struct {
	MinIOEndpoint   string
	MinIOBucket     string
	MinIOAccessKey  string
	MinIOSecretKey  string
	PebbleDirectory string
	ChallTestMgmt   string
}

// StartServices starts MinIO, Pebble, and pebble-challtestsrv.
// A second call in the same test replaces the previous containers.
func StartServices(t *testing.T) Env {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("docker is required to boot MinIO and Pebble")
	}
	StopServices()
	secret, err := randomHex(16)
	if err != nil {
		t.Fatal(err)
	}
	env := Env{
		MinIOEndpoint:   "http://127.0.0.1:" + minioPort,
		MinIOBucket:     "eru-e2e",
		MinIOAccessKey:  "eruadmin",
		MinIOSecretKey:  secret,
		PebbleDirectory: "https://127.0.0.1:" + pebblePort + "/dir",
		ChallTestMgmt:   "http://127.0.0.1:" + challMgmtPort,
	}
	if err := docker("network", "create", networkName); err != nil {
		t.Fatal(err)
	}
	if err := docker(
		"run", "-d", "--name", minioName, "--network", networkName, "--network-alias", "minio",
		"-p", minioPort+":9000",
		"-e", "MINIO_ROOT_USER="+env.MinIOAccessKey,
		"-e", "MINIO_ROOT_PASSWORD="+env.MinIOSecretKey,
		"minio/minio:latest", "server", "/data", "--console-address", ":9001",
	); err != nil {
		t.Fatal(err)
	}
	if err := waitHTTP(env.MinIOEndpoint+"/minio/health/live", 90*time.Second); err != nil {
		logs, _ := dockerOutput("logs", minioName)
		t.Fatalf("minio: %v\n%s", err, logs)
	}
	if err := docker(
		"run", "--rm", "--network", networkName, "--entrypoint", "/bin/sh", "minio/mc:latest", "-c",
		"mc alias set local http://minio:9000 "+env.MinIOAccessKey+" "+env.MinIOSecretKey+" && mc mb -p local/"+env.MinIOBucket,
	); err != nil {
		t.Fatal(err)
	}
	if err := docker(
		"run", "-d", "--name", challName, "--network", networkName, "--network-alias", "challtestsrv",
		"-p", challMgmtPort+":8055", "-p", challDNSPort+":8053",
		"ghcr.io/letsencrypt/pebble-challtestsrv:latest",
		"-defaultIPv6", "", "-defaultIPv4", "127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}
	if err := docker(
		"run", "-d", "--name", pebbleName, "--network", networkName,
		"-p", pebblePort+":14000", "-p", pebbleMgmtPort+":15000",
		"-e", "PEBBLE_VA_NOSLEEP=1",
		"-e", "PEBBLE_VA_ALWAYS_VALID=1",
		"ghcr.io/letsencrypt/pebble:latest",
		"-dnsserver", "challtestsrv:8053",
	); err != nil {
		t.Fatal(err)
	}
	if err := waitRunning(pebbleName, 60*time.Second); err != nil {
		logs, _ := dockerOutput("logs", pebbleName)
		t.Fatalf("pebble: %v\n%s", err, logs)
	}
	if err := waitRunning(challName, 30*time.Second); err != nil {
		logs, _ := dockerOutput("logs", challName)
		t.Fatalf("challtestsrv: %v\n%s", err, logs)
	}
	t.Cleanup(StopServices)
	return env
}

// StopServices removes the e2e containers and network.
func StopServices() {
	_ = docker("rm", "-f", minioName, pebbleName, challName)
	_ = docker("network", "rm", networkName)
}

func docker(args ...string) error {
	_, err := dockerOutput(args...)
	return err
}

func dockerOutput(args ...string) (string, error) {
	cmd := exec.Command("docker", args...) //nolint:gosec // G204: harness invokes docker with fixed subcommands.
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("docker %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

func waitHTTP(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	var last error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			last = err
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("timeout waiting for %s: %w", url, last)
}

func waitRunning(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		out, err := dockerOutput("inspect", "-f", "{{.State.Running}} {{.State.Status}}", name)
		last = strings.TrimSpace(out)
		if err == nil && strings.HasPrefix(last, "true") {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("%s did not stay running (%s)", name, last)
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
