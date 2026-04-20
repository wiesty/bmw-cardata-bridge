package bmw

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// allDescriptors is the full set of BMW telematic descriptor IDs included in the container.
// All are fetched in a single API call — adding more here has no rate-limit cost.
// Fields that don't apply to your vehicle type will be nil (omitempty) in the response.
var allDescriptors = []string{
	// --- Range & mileage (all vehicle types) ---
	"vehicle.vehicle.travelledDistance",
	"vehicle.drivetrain.electricEngine.kombiRemainingElectricRange",
	"vehicle.drivetrain.totalRemainingRange",
	"vehicle.drivetrain.lastRemainingRange",

	// --- Battery & charging (BEV / PHEV) ---
	"vehicle.drivetrain.electricEngine.charging.status",
	"vehicle.drivetrain.electricEngine.charging.level",
	"vehicle.drivetrain.electricEngine.charging.timeToFullyCharged",
	"vehicle.powertrain.electric.battery.charging.power",
	"vehicle.powertrain.electric.battery.stateOfCharge.target",
	"vehicle.powertrain.tractionBattery.charging.port.anyPosition.isPlugged",

	// --- Fuel (ICE / PHEV) ---
	"vehicle.drivetrain.fuelSystem.level",
	"vehicle.drivetrain.fuelSystem.remainingFuel",

	// --- Engine & ignition ---
	"vehicle.drivetrain.engine.isActive",
	"vehicle.drivetrain.engine.isIgnitionOn",
	"vehicle.isMoving",

	// --- Doors, trunk & security ---
	"vehicle.cabin.door.lock.status",
	"vehicle.cabin.door.status",
	"vehicle.cabin.door.row1.driver.isOpen",
	"vehicle.cabin.door.row1.passenger.isOpen",
	"vehicle.cabin.door.row2.driver.isOpen",
	"vehicle.cabin.door.row2.passenger.isOpen",
	"vehicle.body.trunk.isOpen",
	"vehicle.body.hood.isOpen",
	"vehicle.body.lights.isRunningOn",

	// --- Tires ---
	"vehicle.chassis.axle.row1.wheel.left.tire.pressure",
	"vehicle.chassis.axle.row1.wheel.right.tire.pressure",
	"vehicle.chassis.axle.row2.wheel.left.tire.pressure",
	"vehicle.chassis.axle.row2.wheel.right.tire.pressure",
	"vehicle.chassis.axle.wheel.tire.diagnosis",

	// --- Service ---
	"vehicle.status.serviceDistance.next",
	"vehicle.status.checkControlMessages",
}

// containerName is versioned by descriptor count so adding descriptors auto-creates a new container.
var containerName = fmt.Sprintf("bmw-cardata-bridge-%d", len(allDescriptors))

// VehicleData is the in-memory cache populated after each poll.
// Pointer fields are omitted from JSON when nil (not available for this vehicle type).
type VehicleData struct {
	MileageKm  float64   `json:"mileage_km"`
	LastUpdate time.Time `json:"last_update"`

	RangeKm     *float64 `json:"range_km,omitempty"`
	RangeElecKm *float64 `json:"range_electric_km,omitempty"`
	RangeFuelKm *float64 `json:"range_fuel_km,omitempty"`

	BatterySocPct            *float64 `json:"battery_soc_pct,omitempty"`
	BatterySocTargetPct      *float64 `json:"battery_soc_target_pct,omitempty"`
	ChargingStatus           *string  `json:"charging_status,omitempty"`
	ChargingPowerKw          *float64 `json:"charging_power_kw,omitempty"`
	ChargingTimeRemainingMin *float64 `json:"charging_time_remaining_min,omitempty"`
	IsPluggedIn              *bool    `json:"is_plugged_in,omitempty"`

	FuelLevelPct    *float64 `json:"fuel_level_pct,omitempty"`
	FuelLevelLiters *float64 `json:"fuel_level_liters,omitempty"`

	IsMoving     *bool `json:"is_moving,omitempty"`
	IsIgnitionOn *bool `json:"is_ignition_on,omitempty"`
	IsEngineOn   *bool `json:"is_engine_on,omitempty"`

	DoorsLocked *string `json:"doors_locked,omitempty"`
	DoorsStatus *string `json:"doors_status,omitempty"`
	DoorFLOpen  *bool   `json:"door_front_left_open,omitempty"`
	DoorFROpen  *bool   `json:"door_front_right_open,omitempty"`
	DoorRLOpen  *bool   `json:"door_rear_left_open,omitempty"`
	DoorRROpen  *bool   `json:"door_rear_right_open,omitempty"`
	TrunkOpen   *bool   `json:"trunk_open,omitempty"`
	HoodOpen    *bool   `json:"hood_open,omitempty"`
	LightsOn    *bool   `json:"lights_on,omitempty"`

	TirePressureFLkPa *float64 `json:"tire_pressure_fl_kpa,omitempty"`
	TirePressureFRkPa *float64 `json:"tire_pressure_fr_kpa,omitempty"`
	TirePressureRLkPa *float64 `json:"tire_pressure_rl_kpa,omitempty"`
	TirePressureRRkPa *float64 `json:"tire_pressure_rr_kpa,omitempty"`
	TireDiagnosis     *string  `json:"tire_diagnosis,omitempty"`

	ServiceDistanceKm    *float64 `json:"service_distance_km,omitempty"`
	CheckControlMessages *string  `json:"check_control_messages,omitempty"`
}

// Cache is the thread-safe in-memory store for the latest VehicleData.
type Cache struct {
	mu   sync.RWMutex
	data *VehicleData
}

func NewCache() *Cache { return &Cache{} }

func (c *Cache) Set(d VehicleData) {
	c.mu.Lock()
	c.data = &d
	c.mu.Unlock()
}

func (c *Cache) Get() *VehicleData {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.data
}

// VehicleRegistry is a thread-safe map from VIN to Cache for multi-vehicle support.
type VehicleRegistry struct {
	mu     sync.RWMutex
	caches map[string]*Cache
}

func NewRegistry() *VehicleRegistry {
	return &VehicleRegistry{caches: make(map[string]*Cache)}
}

func (r *VehicleRegistry) Add(vin string, c *Cache) {
	r.mu.Lock()
	r.caches[vin] = c
	r.mu.Unlock()
}

func (r *VehicleRegistry) Get(vin string) *Cache {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.caches[vin]
}

// VINs returns all registered VINs in sorted order.
func (r *VehicleRegistry) VINs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	vins := make([]string, 0, len(r.caches))
	for v := range r.caches {
		vins = append(vins, v)
	}
	sort.Strings(vins)
	return vins
}

// state persists VIN, container ID, last poll time, and cached vehicle data across restarts.
type state struct {
	VIN           string       `json:"vin,omitempty"`
	ContainerID   string       `json:"container_id"`
	ContainerName string       `json:"container_name,omitempty"`
	LastPollTime  time.Time    `json:"last_poll_time,omitempty"`
	CachedData    *VehicleData `json:"cached_data,omitempty"`
}

func loadState(path string) (*state, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &state{}, nil
	}
	if err != nil {
		return nil, err
	}
	var s state
	return &s, json.Unmarshal(data, &s)
}

func saveState(path string, s *state) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// Bootstrap loads or discovers a single VIN and container ID, persisting them to state.json.
// Kept for backward compatibility with single-vehicle mode.
func Bootstrap(ctx context.Context, client *Client, dataDir string) (vin, containerID string, err error) {
	vins, cID, err := BootstrapMulti(ctx, client, dataDir, nil)
	if err != nil {
		return "", "", err
	}
	return vins[0], cID, nil
}

// BootstrapMulti discovers/validates VINs and ensures the shared container exists.
// If explicitVINs is non-empty, those VINs are used instead of auto-discovery.
// Container info is persisted to state.json (shared); per-VIN poll state uses state_<VIN>.json.
func BootstrapMulti(ctx context.Context, client *Client, dataDir string, explicitVINs []string) (vins []string, containerID string, err error) {
	statePath := filepath.Join(dataDir, "state.json")
	s, err := loadState(statePath)
	if err != nil {
		return nil, "", fmt.Errorf("load state: %w", err)
	}

	if len(explicitVINs) > 0 {
		vins = explicitVINs
	} else if s.VIN != "" {
		vins = []string{s.VIN}
	} else {
		vin, err := discoverVIN(ctx, client)
		if err != nil {
			return nil, "", fmt.Errorf("discover VIN: %w", err)
		}
		log.Printf("[bmw] discovered VIN: %s", vin)
		vins = []string{vin}
	}

	if s.ContainerID == "" || s.ContainerName != containerName {
		s.ContainerID = ""
		s.ContainerID, err = ensureContainer(ctx, client)
		if err != nil {
			return nil, "", fmt.Errorf("ensure container: %w", err)
		}
		s.ContainerName = containerName
		log.Printf("[bmw] using container: %s (%s)", s.ContainerID, containerName)
	}

	// Persist VIN in state.json for single-vehicle restarts (no BMW_VINS set).
	if len(vins) == 1 {
		s.VIN = vins[0]
	}

	if err := saveState(statePath, s); err != nil {
		log.Printf("[bmw] warning: could not save state: %v", err)
	}

	return vins, s.ContainerID, nil
}

func discoverVIN(ctx context.Context, client *Client) (string, error) {
	mappings, err := client.getMappings(ctx)
	if err != nil {
		return "", err
	}
	if len(mappings) == 0 {
		return "", fmt.Errorf("no vehicles found in account")
	}
	for _, m := range mappings {
		if m.MappingType != nil && *m.MappingType == "PRIMARY" && m.Vin != nil {
			return *m.Vin, nil
		}
	}
	if mappings[0].Vin != nil {
		return *mappings[0].Vin, nil
	}
	return "", fmt.Errorf("no VIN found in mappings")
}

func ensureContainer(ctx context.Context, client *Client) (string, error) {
	if id, err := findContainer(ctx, client); err == nil && id != "" {
		return id, nil
	}
	return client.createContainer(ctx, containerName, allDescriptors)
}

func findContainer(ctx context.Context, client *Client) (string, error) {
	list, err := client.listContainers(ctx)
	if err != nil {
		return "", err
	}
	for _, c := range list {
		if c.Name != nil && *c.Name == containerName && c.ContainerId != nil {
			return *c.ContainerId, nil
		}
	}
	return "", nil
}

// StartPoller starts the polling loop for a single VIN.
// statePath is the full path to the state file used to persist poll time and cached data.
// On restart it checks the last poll time and reuses cached data if the interval hasn't elapsed.
func StartPoller(ctx context.Context, client *Client, vin, containerID string, interval time.Duration, cache *Cache, statePath string) {
	s, _ := loadState(statePath)

	delay := time.Duration(0)
	if s != nil && s.CachedData != nil && !s.LastPollTime.IsZero() {
		elapsed := time.Since(s.LastPollTime)
		if elapsed < interval {
			cache.Set(*s.CachedData)
			delay = interval - elapsed
			log.Printf("[poller] VIN=%s cached data loaded (polled %v ago), next poll in %v",
				vin, elapsed.Round(time.Second), delay.Round(time.Second))
		}
	}

	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return
	}

	poll(ctx, client, vin, containerID, cache, statePath)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			poll(ctx, client, vin, containerID, cache, statePath)
		case <-ctx.Done():
			return
		}
	}
}

func poll(ctx context.Context, client *Client, vin, containerID string, cache *Cache, statePath string) {
	data, err := client.getTelematicData(ctx, vin, containerID)
	if err != nil {
		log.Printf("[poll] VIN=%s error: %v", vin, err)
		return
	}
	if data == nil {
		log.Printf("[poll] VIN=%s empty response", vin)
		return
	}

	d := VehicleData{
		MileageKm:  getFloat(data, "vehicle.vehicle.travelledDistance"),
		LastUpdate: time.Now().UTC(),
	}

	elecRange := getFloatPtr(data, "vehicle.drivetrain.electricEngine.kombiRemainingElectricRange")
	totalRange := getFloatPtr(data, "vehicle.drivetrain.totalRemainingRange")
	fuelRange := getFloatPtr(data, "vehicle.drivetrain.lastRemainingRange")
	d.RangeElecKm = elecRange
	d.RangeFuelKm = fuelRange
	switch {
	case totalRange != nil && *totalRange > 0:
		d.RangeKm = totalRange
	case elecRange != nil && *elecRange > 0:
		d.RangeKm = elecRange
	case fuelRange != nil && *fuelRange > 0:
		d.RangeKm = fuelRange
	}

	d.BatterySocPct = getFloatPtr(data, "vehicle.drivetrain.electricEngine.charging.level")
	d.BatterySocTargetPct = getFloatPtr(data, "vehicle.powertrain.electric.battery.stateOfCharge.target")
	d.ChargingStatus = getStringPtr(data, "vehicle.drivetrain.electricEngine.charging.status")
	d.ChargingPowerKw = getFloatPtr(data, "vehicle.powertrain.electric.battery.charging.power")
	d.ChargingTimeRemainingMin = getFloatPtr(data, "vehicle.drivetrain.electricEngine.charging.timeToFullyCharged")
	d.IsPluggedIn = getBoolPtr(data, "vehicle.powertrain.tractionBattery.charging.port.anyPosition.isPlugged")

	d.FuelLevelPct = getFloatPtr(data, "vehicle.drivetrain.fuelSystem.level")
	d.FuelLevelLiters = getFloatPtr(data, "vehicle.drivetrain.fuelSystem.remainingFuel")

	d.IsMoving = getBoolPtr(data, "vehicle.isMoving")
	d.IsIgnitionOn = getBoolPtr(data, "vehicle.drivetrain.engine.isIgnitionOn")
	d.IsEngineOn = getBoolPtr(data, "vehicle.drivetrain.engine.isActive")

	d.DoorsLocked = getStringPtr(data, "vehicle.cabin.door.lock.status")
	d.DoorsStatus = getStringPtr(data, "vehicle.cabin.door.status")
	d.DoorFLOpen = getBoolPtr(data, "vehicle.cabin.door.row1.driver.isOpen")
	d.DoorFROpen = getBoolPtr(data, "vehicle.cabin.door.row1.passenger.isOpen")
	d.DoorRLOpen = getBoolPtr(data, "vehicle.cabin.door.row2.driver.isOpen")
	d.DoorRROpen = getBoolPtr(data, "vehicle.cabin.door.row2.passenger.isOpen")
	d.TrunkOpen = getBoolPtr(data, "vehicle.body.trunk.isOpen")
	d.HoodOpen = getBoolPtr(data, "vehicle.body.hood.isOpen")
	d.LightsOn = getBoolPtr(data, "vehicle.body.lights.isRunningOn")

	d.TirePressureFLkPa = getFloatPtr(data, "vehicle.chassis.axle.row1.wheel.left.tire.pressure")
	d.TirePressureFRkPa = getFloatPtr(data, "vehicle.chassis.axle.row1.wheel.right.tire.pressure")
	d.TirePressureRLkPa = getFloatPtr(data, "vehicle.chassis.axle.row2.wheel.left.tire.pressure")
	d.TirePressureRRkPa = getFloatPtr(data, "vehicle.chassis.axle.row2.wheel.right.tire.pressure")
	d.TireDiagnosis = getStringPtr(data, "vehicle.chassis.axle.wheel.tire.diagnosis")

	d.ServiceDistanceKm = getFloatPtr(data, "vehicle.status.serviceDistance.next")
	d.CheckControlMessages = getStringPtr(data, "vehicle.status.checkControlMessages")

	cache.Set(d)

	// Persist so restarts can skip a poll if interval hasn't elapsed.
	if s, err := loadState(statePath); err == nil {
		s.VIN = vin
		s.LastPollTime = d.LastUpdate
		s.CachedData = &d
		if err := saveState(statePath, s); err != nil {
			log.Printf("[poll] VIN=%s warning: could not save state: %v", vin, err)
		}
	}

	log.Printf("[poll] ok — VIN=%s", vin)
}

func getFloat(e map[string]TelematicEntry, key string) float64 {
	v := getFloatPtr(e, key)
	if v == nil {
		return 0
	}
	return *v
}

func getFloatPtr(e map[string]TelematicEntry, key string) *float64 {
	entry, ok := e[key]
	if !ok || entry.Value == nil || *entry.Value == "" {
		return nil
	}
	v, err := strconv.ParseFloat(*entry.Value, 64)
	if err != nil {
		return nil
	}
	return &v
}

func getBoolPtr(e map[string]TelematicEntry, key string) *bool {
	entry, ok := e[key]
	if !ok || entry.Value == nil || *entry.Value == "" {
		return nil
	}
	v, err := strconv.ParseBool(*entry.Value)
	if err != nil {
		return nil
	}
	return &v
}

func getStringPtr(e map[string]TelematicEntry, key string) *string {
	entry, ok := e[key]
	if !ok || entry.Value == nil || *entry.Value == "" {
		return nil
	}
	return entry.Value
}

func floatOr(v *float64, fallback float64) float64 {
	if v == nil {
		return fallback
	}
	return *v
}
