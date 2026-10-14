package migrate

// MinimumVersion is the oldest supported nonempty history schema. Older formats
// have no migration path and must be rejected before waiting for upgrade access.
const MinimumVersion = 5
