package idmapreservation

// LockName serializes local isolated idmap allocation and durable migration reservations.
const LockName = "isolated-idmap-reservations"

// Reservation represents a half-open host ID range reserved by an in-flight instance create.
type Reservation struct {
	Base int64
	Size int64
}

var transient = map[int]Reservation{}

// RangesOverlap reports whether two half-open idmap ranges overlap.
func RangesOverlap(baseA int64, sizeA int64, baseB int64, sizeB int64) bool {
	return baseA < baseB+sizeB && baseB < baseA+sizeA
}

// Transient returns a copy of in-flight instance reservations.
// The caller must hold LockName.
func Transient() map[int]Reservation {
	reservations := make(map[int]Reservation, len(transient))
	for instanceID, reservation := range transient {
		reservations[instanceID] = reservation
	}

	return reservations
}

// SetTransient reserves a range for an in-flight instance create.
// The caller must hold LockName.
func SetTransient(instanceID int, reservation Reservation) {
	transient[instanceID] = reservation
}

// ClearTransient releases a matching in-flight reservation.
// The caller must hold LockName.
func ClearTransient(instanceID int, reservation Reservation) {
	current, ok := transient[instanceID]
	if ok && current == reservation {
		delete(transient, instanceID)
	}
}
