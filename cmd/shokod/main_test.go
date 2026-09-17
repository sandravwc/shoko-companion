package main

import "testing"

func ep(id, num int, file, watched bool) episode {
	var e episode
	e.IDs.ID, e.AniDB.EpisodeNumber = id, num
	if file {
		e.Files = append(e.Files, struct{ ID int }{id})
	}
	if watched {
		w := "t"
		e.Watched = &w
	}
	return e
}

func TestShokoProgress(t *testing.T) {
	cases := []struct {
		name string
		eps  []episode
		want int
	}{
		{"none", []episode{ep(1, 1, true, false)}, 0},
		{"gaps ignored", []episode{ep(1, 1, true, true), ep(2, 2, true, false), ep(6, 6, true, true)}, 6},
		{"unsorted", []episode{ep(6, 6, true, true), ep(3, 3, true, true)}, 6},
	}
	for _, c := range cases {
		if got := shokoProgress(c.eps); got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
}

func TestStartAt(t *testing.T) {
	eps := []episode{ep(3, 3, true, false), ep(4, 4, true, false), ep(5, 5, true, false)}
	got := startAt(eps, 4)
	if got[0].IDs.ID != 4 || got[1].IDs.ID != 5 || got[2].IDs.ID != 3 {
		t.Errorf("rotation wrong: %d %d %d", got[0].IDs.ID, got[1].IDs.ID, got[2].IDs.ID)
	}
	if startAt(eps, 99)[0].IDs.ID != 3 {
		t.Error("unknown id should keep order")
	}
}
