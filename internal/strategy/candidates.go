package strategy

// Candidates is the pre-registered set of allocators (Tulip ADR 0004) for a
// universe whose LAST symbol is the safe asset. The live strategy is one of
// them; the others run as shadow books and in the backtest comparison. Adding
// one is an ADR change, not a code tweak: the list is fixed before the data
// that will judge it exists.
func Candidates(syms []string) []Allocator {
	if len(syms) < 2 {
		return nil
	}
	risk, safe := syms[:len(syms)-1], syms[len(syms)-1]
	return []Allocator{
		TrendFilter{Risk: risk, Safe: safe},
		EqualWeight{Symbols: syms},
		TrendEnsemble{Risk: risk, Safe: safe},
		DualMomentum{Risk: risk, Safe: safe},
		InverseVol{Symbols: syms},
	}
}

// ByName finds a pre-registered allocator.
func ByName(syms []string, name string) (Allocator, bool) {
	for _, a := range Candidates(syms) {
		if a.Name() == name {
			return a, true
		}
	}
	return nil, false
}
