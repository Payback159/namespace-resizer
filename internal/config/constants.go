package config

const (
	// AnnotationEnabled controls whether the resizer is enabled for a namespace (default: true)
	AnnotationEnabled = "resizer.io/enabled"

	// AnnotationCPUThreshold sets the CPU threshold percentage (e.g. "80").
	//
	// Deprecated: use AnnotationCPUHeadroom. The value is still honoured and
	// converted to a headroom fraction.
	AnnotationCPUThreshold = "resizer.io/cpu-threshold"
	// AnnotationMemoryThreshold sets the Memory threshold percentage
	//
	// Deprecated: use AnnotationMemoryHeadroom. The value is still honoured
	// and converted to a headroom fraction.
	AnnotationMemoryThreshold = "resizer.io/memory-threshold"
	// AnnotationStorageThreshold sets the Storage threshold percentage
	//
	// Deprecated: use AnnotationStorageHeadroom. The value is still honoured
	// and converted to a headroom fraction.
	AnnotationStorageThreshold = "resizer.io/storage-threshold"

	// AnnotationCPUIncrement sets the CPU increment factor (e.g. "10%")
	//
	// Deprecated: use AnnotationCPUHeadroom. The value is still honoured.
	AnnotationCPUIncrement = "resizer.io/cpu-increment"
	// AnnotationMemoryIncrement sets the Memory increment factor
	//
	// Deprecated: use AnnotationMemoryHeadroom. The value is still honoured.
	AnnotationMemoryIncrement = "resizer.io/memory-increment"
	// AnnotationStorageIncrement sets the Storage increment factor
	//
	// Deprecated: use AnnotationStorageHeadroom. The value is still honoured.
	AnnotationStorageIncrement = "resizer.io/storage-increment"

	// AnnotationCPUHeadroom sets the CPU headroom fraction (e.g. "0.25" or "25%").
	AnnotationCPUHeadroom = "resizer.io/cpu-headroom"
	// AnnotationMemoryHeadroom sets the memory headroom fraction.
	AnnotationMemoryHeadroom = "resizer.io/memory-headroom"
	// AnnotationStorageHeadroom sets the storage headroom fraction.
	AnnotationStorageHeadroom = "resizer.io/storage-headroom"

	// AnnotationTolerance sets the dead band around the target (default "0.15").
	AnnotationTolerance = "resizer.io/tolerance"
	// AnnotationWindowDays sets the observation window length (default "14").
	AnnotationWindowDays = "resizer.io/window-days"
	// AnnotationShrinkCooldownDays sets the shrink cooldown (default "7").
	AnnotationShrinkCooldownDays = "resizer.io/shrink-cooldown-days"
	// AnnotationMaxShrinkStep caps a single shrink (default "0.25").
	AnnotationMaxShrinkStep = "resizer.io/max-shrink-step"
	// AnnotationShrinkPRTTLDays expires an unreviewed shrink PR (default "7").
	AnnotationShrinkPRTTLDays = "resizer.io/shrink-pr-ttl-days"
	// AnnotationShrinkEnabled opts a namespace out of shrinking. It cannot
	// opt in when --enable-shrink is off.
	AnnotationShrinkEnabled = "resizer.io/shrink-enabled"

	// AnnotationAutoMerge controls whether the controller should auto-merge PRs (default: global setting)
	// Values: "true", "false"
	AnnotationAutoMerge = "resizer.io/auto-merge"
)
