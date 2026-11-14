package collect

import (
	"fmt"
	"regexp"
)

// Thermal throttling: what turns a temperature red on the page.
//
// A CPU or GPU is flagged only while heat is holding it back, judged against that chip's own
// limits, never one temperature for every device:
//   - NVIDIA GPUs (nvidia.go): NVML's clock event reasons, the flags nvidia-smi shows as "SW/HW
//     Thermal Slowdown". A power cap, the usual limit at full load, is not heat and is not flagged.
//   - CPUs, and integrated GPUs (which share the CPU's die): the temperature reaching the chip's
//     maximum junction temperature, Tjmax. Ryzen boosts until it reaches Tjmax and then lowers
//     its clocks to stay there, so reaching it is where heat starts to cost performance. Ryzen
//     has no throttle flag software can read without a kernel driver; Intel's coretemp gives
//     the chip's own Tjmax.

// nvThrottleDesc is GPUInfo.Throttle for an NVIDIA GPU whose driver reports clock event reasons.
const nvThrottleDesc = "NVML clock event reasons: SW or HW thermal slowdown"

// tjMaxMargin: a chip held at its limit reads a little either side of it from second to second.
const tjMaxMargin = 1.0

// atLimit reports whether temp has reached limit (°C); a limit of 0 is unknown and never reached.
func atLimit(temp, limit float64) bool { return limit > 0 && temp >= limit-tjMaxMargin }

var (
	reRyzenDesktop = regexp.MustCompile(`^Ryzen [3579] ([79])\d{3}(X3D|X)?$`) // 7000 and 9000 desktop
	reRyzenAIMax   = regexp.MustCompile(`^Ryzen AI Max\+? (PRO )?\d{3}$`)     // Strix Halo
)

// cpuTjMax returns the Tjmax of a CPU from its cleaned model name (CleanCPUName), from AMD's
// published specifications, and says where the figure comes from. ok is false for a model it
// does not know; such a CPU is never flagged rather than flagged against a guess.
func cpuTjMax(model string) (limit float64, src string, ok bool) {
	if reRyzenAIMax.MatchString(model) {
		return 100, "Ryzen AI Max (Strix Halo)", true
	}
	if m := reRyzenDesktop.FindStringSubmatch(model); m != nil {
		switch {
		case m[1] == "7" && m[2] == "X3D":
			return 89, "Ryzen 7000 X3D desktop", true
		case m[1] == "7":
			return 95, "Ryzen 7000 desktop", true
		default:
			return 95, "Ryzen 9000 desktop", true
		}
	}
	return 0, "", false
}

// cpuThrottleDesc is Info.CPUThrottle for a CPU judged against its Tjmax.
func cpuThrottleDesc(limit float64, from string) string {
	return fmt.Sprintf("temperature at its %g °C Tjmax (%s)", limit, from)
}
