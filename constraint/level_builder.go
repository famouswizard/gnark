package constraint

// The main idea here is to find a naive clustering of independent constraints that can be solved in parallel.
//
// We know that at each constraint, we will have at most one unsolved wire.
// (a constraint may have no unsolved wire in which case it is a plain check that the constraint hold,
// or it may additionally have some wires that will be solved by solver hints)
//
// We build a graph of dependency; we say that a wire is solved at a level l
// --> l = max(level_of_dependencies(wire)) + 1
func (system *System) updateLevel(iID int, walkWires WireWalker, level int) {
	if level <= 0 {
		// level 0 represents the inputs of the circuit (public and secret)
		level = -1
	}
	// level = max(dependencies)

	// process all wires of the instruction
	walkWires(func(wire uint32) int {
		return system.processWire(wire, &level)
	})

	// (new) level =  max(dependencies) + 1
	level++

	// mark output wire with level
	for _, wireID := range system.lbOutputs {
		system.lbWireLevel[wireID] = level
	}

	// we can't skip levels, so appending is fine.
	if level >= len(system.Levels) {
		system.Levels = append(system.Levels, []int{iID})
	} else {
		system.Levels[level] = append(system.Levels[level], iID)
	}
	// clean the table. NB! Do not remove or move, this is required to make the
	// compilation deterministic.
	system.lbOutputs = system.lbOutputs[:0]
}

func (system *System) processWire(wireID uint32, maxLevel *int) (level int) {
	if wireID < uint32(system.GetNbPublicVariables()+system.GetNbSecretVariables()) {
		return -1 // ignore inputs
	}
	for int(wireID) >= len(system.lbWireLevel) {
		// we didn't encounter this wire yet, we need to grow b.wireLevels
		system.lbWireLevel = append(system.lbWireLevel, -1)
	}
	if wLevel := system.lbWireLevel[wireID]; wLevel != -1 {
		// we know how to solve this wire, it's a dependency
		if wLevel > *maxLevel {
			*maxLevel = wLevel
		}
		return wLevel
	}
	// this wire is an output to the instruction
	system.lbOutputs = append(system.lbOutputs, wireID)
	return -1
}
