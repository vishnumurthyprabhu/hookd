package store_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"hookd/internal/store"
	"hookd/internal/store/storetest"
)

func TestSettings(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		c := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 123456789, time.UTC)}
		s := db.New(t, store.WithClock(c.now))

		got, err := s.ListSettings(ctx)
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("ListSettings on empty table = %#v, %v; want empty slice", got, err)
		}

		t1 := c.t.Truncate(time.Microsecond)
		if err := s.SetSetting(ctx, "HOOKD_WORKERS", "8"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetSetting(ctx, "HOOKD_DELIVERY_TIMEOUT", "5s"); err != nil {
			t.Fatal(err)
		}
		c.advance(time.Minute)
		t2 := c.t.Truncate(time.Microsecond)
		// Setting it again replaces the value and its time.
		if err := s.SetSetting(ctx, "HOOKD_WORKERS", "2"); err != nil {
			t.Fatal(err)
		}

		want := []store.Setting{
			{Name: "HOOKD_DELIVERY_TIMEOUT", Value: "5s", UpdatedAt: t1},
			{Name: "HOOKD_WORKERS", Value: "2", UpdatedAt: t2},
		}
		got, err = s.ListSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("ListSettings = %+v, want %+v", got, want)
		}

		if err := s.DeleteSetting(ctx, "HOOKD_WORKERS"); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteSetting(ctx, "HOOKD_WORKERS"); err != nil {
			t.Fatalf("deleting a setting that is not saved: %v", err)
		}
		got, err = s.ListSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want[:1]) {
			t.Fatalf("ListSettings after delete = %+v, want %+v", got, want[:1])
		}

		// Another instance sees the saved settings.
		got, err = db.Open(t).ListSettings(ctx)
		if err != nil || !reflect.DeepEqual(got, want[:1]) {
			t.Fatalf("ListSettings from a new connection = %+v, %v", got, err)
		}
	})
}
