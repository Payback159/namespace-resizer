package config

const (
	// AnnotationEnabled controls whether the resizer is enabled for a namespace (default: true)
	AnnotationEnabled = "resizer.io/enabled"

	// AnnotationCPUThreshold sets the CPU threshold percentage (e.g. "80")
	AnnotationCPUThreshold = "resizer.io/cpu-threshold"
	// AnnotationMemoryThreshold sets the Memory threshold percentage
	AnnotationMemoryThreshold = "resizer.io/memory-threshold"
	// AnnotationStorageThreshold sets the Storage threshold percentage
	AnnotationStorageThreshold = "resizer.io/storage-threshold"

	// AnnotationCPUIncrement sets the CPU increment factor (e.g. "10%")
	AnnotationCPUIncrement = "resizer.io/cpu-increment"
	// AnnotationMemoryIncrement sets the Memory increment factor
	AnnotationMemoryIncrement = "resizer.io/memory-increment"
	// AnnotationStorageIncrement sets the Storage increment factor
	AnnotationStorageIncrement = "resizer.io/storage-increment"

	// AnnotationAutoMerge controls whether the controller should auto-merge PRs (default: global setting)
	// Values: "true", "false"
	AnnotationAutoMerge = "resizer.io/auto-merge"
)
