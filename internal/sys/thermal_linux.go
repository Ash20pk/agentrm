package sys

// ThermalLevel is not read on Linux: thermal zones and their trip points differ too much from
// one machine to the next to rate them the way macOS does. -1 means unknown, and nothing acts on it.
func ThermalLevel() int { return -1 }
