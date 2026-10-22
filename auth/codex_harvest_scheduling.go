package auth

import "slices"

func (a *Account) HarvestOwnsPause() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.HarvestSchedulingPaused
}
func (a *Account) HarvestInGroups(ids map[int64]struct{}) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, id := range a.GroupIDs {
		if _, ok := ids[id]; ok {
			return true
		}
	}
	for _, id := range a.HarvestSchedulingGroups {
		if _, ok := ids[id]; ok {
			return true
		}
	}
	return false
}
func (s *Store) ApplyHarvestScheduling(id int64, paused, modelBlocked bool, originalGroups []int64) {
	a := s.FindByID(id)
	if a == nil {
		return
	}
	a.mu.Lock()
	a.HarvestSchedulingPaused = paused
	a.HarvestModelBlocked = modelBlocked
	a.HarvestSchedulingGroups = slices.Clone(originalGroups)
	a.mu.Unlock()
}
