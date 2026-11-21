package models

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestListerCaches(t *testing.T) {
	calls := 0
	fetch := func(context.Context) ([]string, error) {
		calls++
		return []string{" gpt-6-luna ", ""}, nil
	}
	l := &Lister{}
	for range 2 {
		got, err := l.List(context.Background(), fetch)
		if err != nil || !reflect.DeepEqual(got, []string{"gpt-6-luna"}) {
			t.Fatalf("got %v %v", got, err)
		}
	}
	if calls != 1 {
		t.Fatalf("fetched %d times", calls)
	}
	if _, err := (&Lister{}).List(context.Background(), func(context.Context) ([]string, error) { return nil, errors.New("down") }); err == nil {
		t.Fatal("fetch error must be returned")
	}
}
