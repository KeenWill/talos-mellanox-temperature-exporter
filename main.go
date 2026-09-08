// SPDX-License-Identifier: MIT
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultInterval    = time.Minute
	defaultTimeout     = 20 * time.Second
	maximumOutputBytes = 4096
	maximumDevices     = 64
	minimumTemperature = -40
	maximumTemperature = 150
)

var pciPattern = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)

type pciAddress string

type device struct {
	Address    pciAddress
	Interfaces string
	DeviceID   string
	Firmware   string
	BoardID    string
}

type sample struct {
	Device      device
	Temperature float64
	Success     bool
	LastSuccess time.Time
}

type snapshot struct {
	Samples          []sample
	DiscoverySuccess bool
	Completed        time.Time
}

type collector struct {
	mu         sync.RWMutex
	current    snapshot
	sysfsRoot  string
	selected   []pciAddress
	command    string
	timeout    time.Duration
	maximumAge time.Duration
}

func readAttribute(path string) (string, error) {
	data, err := os.ReadFile(path)
	return strings.TrimSpace(string(data)), err
}

func identifyDevice(root string, address pciAddress) (device, error) {
	result := device{Address: address}
	directory := filepath.Join(root, "bus/pci/devices", string(address))
	vendor, err := readAttribute(filepath.Join(directory, "vendor"))
	if err != nil {
		return result, err
	}
	if vendor != "0x15b3" {
		return result, fmt.Errorf("not a Mellanox device")
	}
	driver, err := os.Readlink(filepath.Join(directory, "driver"))
	if err != nil {
		return result, err
	}
	if filepath.Base(driver) != "mlx5_core" {
		return result, fmt.Errorf("not bound to mlx5_core")
	}
	if _, err := os.Lstat(filepath.Join(directory, "physfn")); err == nil {
		return result, fmt.Errorf("virtual functions are excluded")
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	result.DeviceID, err = readAttribute(filepath.Join(directory, "device"))
	if err != nil {
		return result, err
	}
	interfaces, err := os.ReadDir(filepath.Join(directory, "net"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	names := make([]string, 0, len(interfaces))
	for _, entry := range interfaces {
		names = append(names, entry.Name())
	}
	result.Interfaces = strings.Join(names, ",")
	// Metadata is optional: a readable ASIC must not depend on RDMA configuration.
	adapters, _ := os.ReadDir(filepath.Join(directory, "infiniband"))
	if len(adapters) > 0 {
		result.Firmware, _ = readAttribute(filepath.Join(directory, "infiniband", adapters[0].Name(), "fw_ver"))
		result.BoardID, _ = readAttribute(filepath.Join(directory, "infiniband", adapters[0].Name(), "board_id"))
	}
	return result, nil
}

func discoverDevices(root string, selected []pciAddress) ([]pciAddress, error) {
	if len(selected) > 0 {
		return append([]pciAddress(nil), selected...), nil
	}
	entries, err := os.ReadDir(filepath.Join(root, "bus/pci/devices"))
	if err != nil {
		return nil, err
	}
	addresses := make([]pciAddress, 0)
	for _, entry := range entries {
		if !pciPattern.MatchString(entry.Name()) {
			continue
		}
		address := pciAddress(entry.Name())
		directory := filepath.Join(root, "bus/pci/devices", entry.Name())
		vendor, err := readAttribute(filepath.Join(directory, "vendor"))
		if err != nil {
			return nil, err
		}
		if vendor != "0x15b3" {
			continue
		}
		driver, err := os.Readlink(filepath.Join(directory, "driver"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if filepath.Base(driver) != "mlx5_core" {
			continue
		}
		if _, err := os.Lstat(filepath.Join(directory, "physfn")); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		addresses = append(addresses, address)
	}
	if len(addresses) > maximumDevices {
		return nil, fmt.Errorf("more than %d devices; use --device to limit collection", maximumDevices)
	}
	return addresses, nil
}

// The vendor command normally emits a single number. Bound output even if a
// replacement binary malfunctions.
type boundedOutput struct{ data []byte }

func (output *boundedOutput) Write(data []byte) (int, error) {
	if len(output.data)+len(data) > maximumOutputBytes {
		return 0, fmt.Errorf("temperature command output exceeds %d bytes", maximumOutputBytes)
	}
	output.data = append(output.data, data...)
	return len(data), nil
}

func parseTemperature(output string) (float64, error) {
	value, err := strconv.ParseFloat(strings.TrimSpace(output), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < minimumTemperature || value > maximumTemperature {
		return 0, fmt.Errorf("invalid temperature response")
	}
	return value, nil
}

func readTemperature(ctx context.Context, command string, address pciAddress, timeout time.Duration) (float64, error) {
	queryContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	query := exec.CommandContext(queryContext, command, "-d", string(address), "--no-modules")
	query.WaitDelay = time.Second
	output := &boundedOutput{}
	query.Stdout = output
	if err := query.Run(); err != nil {
		return 0, fmt.Errorf("temperature query failed: %w", err)
	}
	return parseTemperature(string(output.data))
}

func (collector *collector) collect(ctx context.Context) {
	collector.mu.RLock()
	previous := collector.current
	collector.mu.RUnlock()
	prior := make(map[pciAddress]sample, len(previous.Samples))
	for _, reading := range previous.Samples {
		prior[reading.Device.Address] = reading
	}
	addresses, err := discoverDevices(collector.sysfsRoot, collector.selected)
	next := snapshot{DiscoverySuccess: err == nil}
	if err != nil {
		log.Printf("PCI discovery failed: %v", err)
		// Preserve failed identities and last-success times, never stale temperatures.
		for _, reading := range previous.Samples {
			reading.Success = false
			next.Samples = append(next.Samples, reading)
		}
	}
	for _, address := range addresses {
		reading := sample{Device: device{Address: address}, LastSuccess: prior[address].LastSuccess}
		identity, queryError := identifyDevice(collector.sysfsRoot, address)
		if queryError == nil {
			reading.Device = identity
			reading.Temperature, queryError = readTemperature(ctx, collector.command, address, collector.timeout)
		}
		if queryError == nil {
			reading.Success = true
			reading.LastSuccess = time.Now()
		} else {
			log.Printf("PCI %s: %v", address, queryError)
		}
		next.Samples = append(next.Samples, reading)
		if ctx.Err() != nil {
			return
		}
	}
	next.Completed = time.Now()
	collector.mu.Lock()
	collector.current = next
	collector.mu.Unlock()
}

func metricLabel(value string) string {
	return strings.NewReplacer("\\", "\\\\", "\n", "\\n", "\"", "\\\"").Replace(value)
}

func boolValue(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (collector *collector) metrics(now time.Time) string {
	collector.mu.RLock()
	state := collector.current
	collector.mu.RUnlock()
	fresh := !state.Completed.IsZero() && now.Sub(state.Completed) <= collector.maximumAge
	successful := fresh && state.DiscoverySuccess && len(state.Samples) > 0
	var output strings.Builder
	output.WriteString("# HELP mellanox_temperature_celsius NIC ASIC temperature in Celsius; absent on failure or stale collection.\n# TYPE mellanox_temperature_celsius gauge\n")
	output.WriteString("# HELP mellanox_temperature_device_info PCI function identity; interfaces can share a function.\n# TYPE mellanox_temperature_device_info gauge\n")
	output.WriteString("# HELP mellanox_temperature_device_collection_success Whether this device has a fresh successful reading.\n# TYPE mellanox_temperature_device_collection_success gauge\n")
	output.WriteString("# HELP mellanox_temperature_device_last_success_timestamp_seconds Last successful read time; zero before first success.\n# TYPE mellanox_temperature_device_last_success_timestamp_seconds gauge\n")
	for _, reading := range state.Samples {
		valid := fresh && reading.Success && now.Sub(reading.LastSuccess) <= collector.maximumAge
		successful = successful && valid
		labels := fmt.Sprintf("pci_address=\"%s\"", metricLabel(string(reading.Device.Address)))
		fmt.Fprintf(&output, "mellanox_temperature_device_info{%s,interfaces=\"%s\",device_id=\"%s\",firmware=\"%s\",board_id=\"%s\"} 1\n", labels, metricLabel(reading.Device.Interfaces), metricLabel(reading.Device.DeviceID), metricLabel(reading.Device.Firmware), metricLabel(reading.Device.BoardID))
		fmt.Fprintf(&output, "mellanox_temperature_device_collection_success{%s} %d\n", labels, boolValue(valid))
		lastSuccess := int64(0)
		if !reading.LastSuccess.IsZero() {
			lastSuccess = reading.LastSuccess.Unix()
		}
		fmt.Fprintf(&output, "mellanox_temperature_device_last_success_timestamp_seconds{%s} %d\n", labels, lastSuccess)
		if valid {
			fmt.Fprintf(&output, "mellanox_temperature_celsius{%s} %g\n", labels, reading.Temperature)
		}
	}
	fmt.Fprintf(&output, "# HELP mellanox_temperature_collection_success Whether discovery and all selected device reads succeeded recently; zero with no devices.\n# TYPE mellanox_temperature_collection_success gauge\nmellanox_temperature_collection_success %d\n", boolValue(successful))
	fmt.Fprintf(&output, "# HELP mellanox_temperature_discovery_success Whether the latest PCI discovery succeeded.\n# TYPE mellanox_temperature_discovery_success gauge\nmellanox_temperature_discovery_success %d\n", boolValue(fresh && state.DiscoverySuccess))
	fmt.Fprintf(&output, "# HELP mellanox_temperature_devices Number of devices in the latest collection snapshot.\n# TYPE mellanox_temperature_devices gauge\nmellanox_temperature_devices %d\n", len(state.Samples))
	return output.String()
}

func (collector *collector) serveHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	switch request.URL.Path {
	case "/metrics":
		response.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		response.Header().Set("Cache-Control", "no-store")
		if request.Method == http.MethodGet {
			_, _ = response.Write([]byte(collector.metrics(time.Now())))
		}
	case "/healthz":
		response.WriteHeader(http.StatusOK)
	default:
		http.NotFound(response, request)
	}
}

type addressFlags []pciAddress

func (addresses *addressFlags) String() string { return fmt.Sprint([]pciAddress(*addresses)) }
func (addresses *addressFlags) Set(value string) error {
	if !pciPattern.MatchString(value) {
		return fmt.Errorf("expected lowercase PCI address dddd:bb:ss.f")
	}
	for _, existing := range *addresses {
		if string(existing) == value {
			return fmt.Errorf("duplicate PCI address")
		}
	}
	if len(*addresses) >= maximumDevices {
		return fmt.Errorf("at most %d devices supported", maximumDevices)
	}
	*addresses = append(*addresses, pciAddress(value))
	return nil
}

func environmentDuration(name string, fallback time.Duration) (time.Duration, error) {
	value, present := os.LookupEnv(name)
	if !present {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return duration, nil
}

func run() error {
	defaultListen := ":9835"
	if configured, present := os.LookupEnv("MELLANOX_TEMPERATURE_LISTEN_ADDRESS"); present {
		defaultListen = configured
	}
	pollingInterval, err := environmentDuration("MELLANOX_TEMPERATURE_POLL_INTERVAL", defaultInterval)
	if err != nil {
		return err
	}
	queryTimeout, err := environmentDuration("MELLANOX_TEMPERATURE_QUERY_TIMEOUT", defaultTimeout)
	if err != nil {
		return err
	}
	listenAddress := flag.String("listen-address", defaultListen, "HTTP listen address; restrict reachability to monitoring clients")
	interval := flag.Duration("interval", pollingInterval, "Delay between completed polling cycles")
	timeout := flag.Duration("timeout", queryTimeout, "Maximum duration of each hardware query")
	root := flag.String("sysfs-root", "/sys", "PCI inventory root (the vendor query still uses native /sys)")
	command := flag.String("temperature-command", "/usr/local/bin/mstmget_temp", "Path to the pinned NVIDIA mstflint temperature reader")
	var addresses addressFlags
	flag.Var(&addresses, "device", "Optional PCI function allowlist; repeat for multiple functions")
	flag.Parse()
	if len(addresses) == 0 {
		if configured := os.Getenv("MELLANOX_TEMPERATURE_PCI_DEVICES"); configured != "" {
			for _, address := range strings.Split(configured, ",") {
				if err := addresses.Set(strings.TrimSpace(address)); err != nil {
					return fmt.Errorf("MELLANOX_TEMPERATURE_PCI_DEVICES: %w", err)
				}
			}
		}
	}
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if *interval < time.Second || *timeout < time.Second || *timeout > *interval {
		return fmt.Errorf("require interval >= timeout >= 1s")
	}
	sort.Slice(addresses, func(first, second int) bool { return addresses[first] < addresses[second] })
	if !filepath.IsAbs(*command) {
		return fmt.Errorf("temperature-command must be an absolute path")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	collector := &collector{sysfsRoot: *root, selected: addresses, command: *command, timeout: *timeout, maximumAge: 2**interval + *timeout}
	listener, err := net.Listen("tcp", *listenAddress)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{Handler: http.HandlerFunc(collector.serveHTTP), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	pollingStopped := make(chan struct{})
	go func() {
		defer close(pollingStopped)
		for {
			collector.collect(ctx)
			select {
			case <-ctx.Done():
				return
			case <-time.After(*interval):
			}
		}
	}()
	go func() { <-ctx.Done(); _ = server.Close() }()
	log.Printf("listening on %s; polling interval %s, query timeout %s", listener.Addr(), *interval, *timeout)
	serveError := server.Serve(listener)
	cancel()
	<-pollingStopped
	if !errors.Is(serveError, http.ErrServerClosed) {
		return serveError
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
