package model

// SourceWeightMetric exposes one source's own daily data. It never falls back
// to the other site or another day. Last successful values are retained with
// weight_valid=false when that source's most recent attempt failed.
func (m Metric) SourceWeightMetric(source string) (Metric, bool) {
	if source != "aizhan" && source != "chinaz" {
		return Metric{}, false
	}
	if snapshot, ok := m.WeightSnapshots[source]; ok {
		out := Metric{}
		if snapshot.Metric != nil {
			out = *snapshot.Metric
		}
		out.ID = m.ID
		out.DomainID = m.DomainID
		out.Domain = m.Domain
		out.SnapshotDate = m.SnapshotDate
		out.WeightSource = source
		valid := snapshot.Valid
		out.WeightValid = &valid
		out.WeightSnapshots = map[string]WeightSnapshot{source: snapshot}
		return out, true
	}
	// A known legacy source is safe to expose before its background migration.
	if m.WeightSource == source {
		m.WeightSnapshots = nil
		return m, true
	}
	return Metric{}, false
}
