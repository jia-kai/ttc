package tool

// NormalizeTimeoutSeconds resolves an explicit zero to the finite maximum, then
// clips other values to [1, maximum]. Omission preserves the caller's default.
// Timeout properties using this helper must not use automatic schema clipping:
// zero must be interpreted before it can be clipped to the minimum.
func NormalizeTimeoutSeconds(value *int, maximum int) []InputWarning {
	if value == nil {
		return nil
	}
	if *value == 0 {
		*value = maximum
		return AdjustedInput("timeout_seconds", 0, maximum)
	}
	return ClipInteger("timeout_seconds", value, 1, maximum)
}
