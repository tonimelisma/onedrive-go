// SQL encoding helpers shared by the store's table writers. They live here
// rather than beside any one table so no single table's file becomes the
// implicit owner of the encoding the whole family depends on.

package syncstore

// boolInt encodes a boolean column: 1 for true, 0 for false.
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
