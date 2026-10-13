//go:build !darwin && !linux

package relaystatus

import "time"

type writer struct{}

func newWriter(string) (*writer, error)  { return nil, ErrUnavailable }
func (*writer) write(Snapshot) error     { return ErrUnavailable }
func (*writer) close()                   {}
func Read(string, time.Time) Observation { return Observation{State: "unavailable"} }
