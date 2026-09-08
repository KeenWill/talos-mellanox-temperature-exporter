// SPDX-License-Identifier: MIT
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The test executable doubles as a fake vendor reader, requiring no installed
// command, network, root access or real hardware.
func TestMain(tests *testing.M) {
	if mode := os.Getenv("TEMPERATURE_TEST_COMMAND"); mode != "" {
		switch mode {
		case "success":
			if len(os.Args) != 4 || os.Args[1] != "-d" || os.Args[3] != "--no-modules" {
				os.Exit(2)
			}
			fmt.Println("68")
		case "failure":
			os.Exit(1)
		case "invalid":
			fmt.Println("unexpected response")
		case "large":
			fmt.Println(strings.Repeat("0", maximumOutputBytes+1))
		case "timeout":
			time.Sleep(5 * time.Second)
		}
		os.Exit(0)
	}
	// Child race-instrumented fake commands must exit immediately after writing.
	if err := os.Setenv("GORACE", "atexit_sleep_ms=0"); err != nil {
		panic(err)
	}
	os.Exit(tests.Run())
}

func createDevice(t *testing.T, root, address, vendor, driver string) string {
	t.Helper()
	directory := filepath.Join(root, "bus/pci/devices", address)
	for _, child := range []string{"net/eth0", "infiniband/mlx5_0"} {
		if err := os.MkdirAll(filepath.Join(directory, child), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for name, value := range map[string]string{"vendor": vendor, "device": "0x1013", "infiniband/mlx5_0/fw_ver": "example-firmware", "infiniband/mlx5_0/board_id": "example-board"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(value+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../../drivers/"+driver, filepath.Join(directory, "driver")); err != nil {
		t.Fatal(err)
	}
	return directory
}

func TestDiscoveryExcludesVirtualFunctionsAndOtherDrivers(t *testing.T) {
	root := t.TempDir()
	createDevice(t, root, "0000:01:00.0", "0x15b3", "mlx5_core")
	virtual := createDevice(t, root, "0000:01:00.1", "0x15b3", "mlx5_core")
	if err := os.Symlink("../0000:01:00.0", filepath.Join(virtual, "physfn")); err != nil {
		t.Fatal(err)
	}
	createDevice(t, root, "0000:02:00.0", "0x8086", "igc")
	createDevice(t, root, "0000:03:00.0", "0x15b3", "vfio-pci")
	addresses, err := discoverDevices(root, nil)
	if err != nil || len(addresses) != 1 || addresses[0] != "0000:01:00.0" {
		t.Fatalf("addresses=%v error=%v", addresses, err)
	}
}

func TestAllowlistDoesNotBypassPhysicalDeviceValidation(t *testing.T) {
	root := t.TempDir()
	directory := createDevice(t, root, "0000:01:00.0", "0x15b3", "mlx5_core")
	if err := os.Symlink("../0000:02:00.0", filepath.Join(directory, "physfn")); err != nil {
		t.Fatal(err)
	}
	_, err := identifyDevice(root, "0000:01:00.0")
	if err == nil {
		t.Fatal("VF accepted")
	}
}

func TestIdentityKeepsSharedInterfacesInOnePCIFunction(t *testing.T) {
	root := t.TempDir()
	directory := createDevice(t, root, "0000:01:00.0", "0x15b3", "mlx5_core")
	if err := os.Mkdir(filepath.Join(directory, "net/eth1"), 0755); err != nil {
		t.Fatal(err)
	}
	identity, err := identifyDevice(root, "0000:01:00.0")
	if err != nil || identity.Interfaces != "eth0,eth1" || identity.Firmware != "example-firmware" {
		t.Fatalf("identity=%+v error=%v", identity, err)
	}
}

func TestTemperatureParsingRejectsMalformedOrUnreasonableValues(t *testing.T) {
	for _, value := range []string{"", "NaN", "+Inf", "-Inf", "151", "-41", "68 extra", "68\n69"} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseTemperature(value); err == nil {
				t.Fatalf("accepted %q", value)
			}
		})
	}
	for _, value := range []string{"68", " 68.5\n", "0", "-5"} {
		if _, err := parseTemperature(value); err != nil {
			t.Fatalf("rejected %q", value)
		}
	}
}

func TestVendorCommandIsBoundedAndUsesASICOnlyArguments(t *testing.T) {
	for _, mode := range []string{"success", "failure", "invalid", "large", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("TEMPERATURE_TEST_COMMAND", mode)
			timeout := 3 * time.Second
			if mode == "timeout" {
				timeout = 100 * time.Millisecond
			}
			value, err := readTemperature(context.Background(), os.Args[0], "0000:01:00.0", timeout)
			if mode == "success" {
				if err != nil || value != 68 {
					t.Fatalf("value=%v error=%v", value, err)
				}
			} else if err == nil {
				t.Fatal("expected query failure")
			}
		})
	}
}

func TestFailedReadRemovesTemperatureButPreservesLastSuccess(t *testing.T) {
	root := t.TempDir()
	createDevice(t, root, "0000:01:00.0", "0x15b3", "mlx5_core")
	collection := &collector{sysfsRoot: root, command: os.Args[0], timeout: time.Second, maximumAge: time.Minute}
	t.Setenv("TEMPERATURE_TEST_COMMAND", "success")
	collection.collect(context.Background())
	if !strings.Contains(collection.metrics(time.Now()), "mellanox_temperature_celsius{pci_address=\"0000:01:00.0\"} 68") {
		t.Fatal("missing temperature")
	}
	previousSuccess := collection.current.Samples[0].LastSuccess
	t.Setenv("TEMPERATURE_TEST_COMMAND", "failure")
	collection.collect(context.Background())
	output := collection.metrics(time.Now())
	if strings.Contains(output, "\nmellanox_temperature_celsius{") {
		t.Fatal("failed device still exports temperature")
	}
	if !strings.Contains(output, "mellanox_temperature_collection_success 0") {
		t.Fatal("failure marked healthy")
	}
	if !collection.current.Samples[0].LastSuccess.Equal(previousSuccess) {
		t.Fatal("last success changed on failure")
	}
}

func TestStaleAndEmptySnapshotsAreUnhealthy(t *testing.T) {
	now := time.Now()
	collection := &collector{maximumAge: time.Minute}
	if !strings.Contains(collection.metrics(now), "mellanox_temperature_collection_success 0") {
		t.Fatal("empty snapshot marked healthy")
	}
	collection.current = snapshot{DiscoverySuccess: true, Completed: now.Add(-2 * time.Minute), Samples: []sample{{Device: device{Address: "0000:01:00.0"}, Success: true, Temperature: 68, LastSuccess: now.Add(-2 * time.Minute)}}}
	output := collection.metrics(now)
	if strings.Contains(output, "\nmellanox_temperature_celsius{") {
		t.Fatal("stale temperature remains exported")
	}
	if !strings.Contains(output, "mellanox_temperature_collection_success 0") {
		t.Fatal("stale snapshot marked healthy")
	}
}

func TestDiscoveryFailureInvalidatesPreviousReadings(t *testing.T) {
	root := t.TempDir()
	createDevice(t, root, "0000:01:00.0", "0x15b3", "mlx5_core")
	collection := &collector{sysfsRoot: root, command: os.Args[0], timeout: time.Second, maximumAge: time.Minute}
	t.Setenv("TEMPERATURE_TEST_COMMAND", "success")
	collection.collect(context.Background())
	collection.sysfsRoot = filepath.Join(root, "missing")
	collection.collect(context.Background())
	output := collection.metrics(time.Now())
	if !strings.Contains(output, "mellanox_temperature_discovery_success 0") || strings.Contains(output, "\nmellanox_temperature_celsius{") {
		t.Fatal(output)
	}
}

func TestMetricEscapingAndHTTPRoutes(t *testing.T) {
	if got := metricLabel("quote\"slash\\line\n"); got != "quote\\\"slash\\\\line\\n" {
		t.Fatalf("escaped=%q", got)
	}
	collection := &collector{maximumAge: time.Minute}
	for _, test := range []struct {
		method, path string
		status       int
	}{{"GET", "/metrics", 200}, {"HEAD", "/metrics", 200}, {"GET", "/healthz", 200}, {"GET", "/unknown", 404}, {"POST", "/metrics", 405}} {
		recorder := httptest.NewRecorder()
		collection.serveHTTP(recorder, httptest.NewRequest(test.method, test.path, nil))
		if recorder.Code != test.status {
			t.Fatalf("%s %s: %d", test.method, test.path, recorder.Code)
		}
		if test.method == http.MethodHead && recorder.Body.Len() != 0 {
			t.Fatal("HEAD returned body")
		}
	}
}

func TestAllowlistRejectsDuplicatesAndPathTraversal(t *testing.T) {
	var flags addressFlags
	for _, value := range []string{"../config", "01:00.0", "0000:01:00.8", "0000:01:00.0;echo hi"} {
		if err := flags.Set(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	if err := flags.Set("0000:01:00.0"); err != nil {
		t.Fatal(err)
	}
	if err := flags.Set("0000:01:00.0"); err == nil {
		t.Fatal("duplicate accepted")
	}
}
