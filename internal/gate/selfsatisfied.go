package gate

import "fmt"

// ReasonSelfSatisfied is the closed-set code for a specification that implements
// its own subject.
const ReasonSelfSatisfied = "implementation-inside-the-tests"

// SelfSatisfiedScript refuses a specification that implements the contract it is
// supposed to specify.
//
// THE TESTS MUST EXERCISE THE DEVELOPER'S CODE, and this is the one way they can
// look correct while exercising nothing.
//
// Measured on run 93. types/types.go declared `type Store interface` with
// Create, Get, Update and List; store_test.go then declared inMemoryStore with
// all four methods and a newStore() returning it, ahead of any test function.
// The ticket became unsatisfiable in both directions: an implementation in
// store.go collides with the one in the tests and will not compile, and no
// implementation at all leaves the suite passing against the author's own code.
// The developer read it correctly — "remove duplicate store implementation from
// store.go (it lives in store_test.go)" — and oscillated for 43 turns, because
// it may not edit the file holding the cause.
//
// NOTHING ELSE COULD SEE IT. SpecAlreadyBuilt counts exported declarations in
// NON-test files, and there were none. SpecVacuous needs the suite to pass, and
// the sibling sections were legitimately red on undefined symbols, which masked
// it. The fixture-literal gate looks for test inputs echoed in production code,
// which is the opposite direction.
//
// MATCHED BY THE FULL METHOD SET, not by a name or a count. A fixture that
// implements every method of a declared interface IS the implementation,
// whatever it is called; one that implements some of them is a stub standing in
// for a collaborator, which is ordinary and stays allowed.
func SelfSatisfiedScript() string {
	return fmt.Sprintf(`
echo '--- an implementation of the declared contract, inside the tests ---'
ifaces=$(find . -path ./.git -prune -o -name '*.go' -print 2>/dev/null | xargs grep -l 'blacksmith:declarations' 2>/dev/null || true)
bad=""
if [ -n "$ifaces" ]; then
  # Every interface declared in the architect's files, and its method names.
  for f in $ifaces; do
    awk '/^type [A-Za-z_][A-Za-z0-9_]* interface \{/ {name=$2; inside=1; n=0; next}
         inside && /^\}/ {if (n>0) print name"\t"n"\t"methods; inside=0; methods=""; next}
         inside && /^\t[A-Z][A-Za-z0-9_]*\(/ {m=$0; sub(/\(.*/,"",m); sub(/^\t/,"",m);
                                              methods=methods" "m; n++}' "$f"
  done > /tmp/.gate-ifaces || true
fi
if [ -s /tmp/.gate-ifaces ]; then
  while IFS="$(printf '\t')" read -r iface count methods; do
    [ "$count" -ge 2 ] || continue
    for t in $(find . -name '*_test.go' -not -path './.git/*' 2>/dev/null); do
      have=0
      for m in $methods; do
        grep -qE "^func \([a-zA-Z_][A-Za-z0-9_]* \*?[A-Za-z_][A-Za-z0-9_]*\) $m\(" "$t" && have=$((have+1))
      done
      if [ "$have" -eq "$count" ]; then
        bad="$bad $t($iface)"
      fi
    done
  done < /tmp/.gate-ifaces
fi
if [ -n "$bad" ]; then
  echo "These test files IMPLEMENT a declared interface rather than specifying it:$bad"
  echo
  echo "A test file carrying every method of an interface from the declarations is"
  echo "the implementation, not a fixture. The developer cannot then build the thing"
  echo "the ticket asks for: its own version collides with yours and will not compile,"
  echo "and without one the suite passes against your code rather than its own."
  echo "Delete the implementation from the test and call the constructor the"
  echo "declarations name; the developer writes what it does."
  echo '%s%s'
  exit 1
fi
echo 'no test file implements a declared interface'
`, Marker, ReasonSelfSatisfied)
}
