package pgdb

import "testing"

func TestResultJSON(t *testing.T) {
	r := &Result{Columns: []string{"b", "a"}, Rows: [][]any{{"<x> & y", nil}}}
	got, err := JSON(r)
	if err != nil {
		t.Fatal(err)
	}
	if want := `[{"b":"<x> & y","a":null}]`; string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}
}
