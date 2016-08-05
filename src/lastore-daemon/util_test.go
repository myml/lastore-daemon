/**
 * Copyright (C) 2015 Deepin Technology Co., Ltd.
 *
 * This program is free software; you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation; either version 3 of the License, or
 * (at your option) any later version.
 **/

package main

import "testing"
import C "gopkg.in/check.v1"
import "internal/system"
import "fixme/pkg_recommend"

type testWrap struct{}

func Test(t *testing.T) { C.TestingT(t) }
func init() {
	C.Suite(&testWrap{})
}

func (*testWrap) TestTranisition(c *C.C) {
	var data = []struct {
		from  system.Status
		to    system.Status
		valid bool
	}{
		{system.ReadyStatus, system.ReadyStatus, false},
		{system.ReadyStatus, system.RunningStatus, true},
		{system.ReadyStatus, system.FailedStatus, false},
		{system.ReadyStatus, system.SucceedStatus, false},
		{system.ReadyStatus, system.PausedStatus, true},
		{system.ReadyStatus, system.EndStatus, false},

		{system.RunningStatus, system.ReadyStatus, false},
		{system.RunningStatus, system.RunningStatus, false},
		{system.RunningStatus, system.FailedStatus, true},
		{system.RunningStatus, system.SucceedStatus, true},
		{system.RunningStatus, system.PausedStatus, true},
		{system.RunningStatus, system.EndStatus, false},

		{system.FailedStatus, system.ReadyStatus, true},
		{system.FailedStatus, system.RunningStatus, false},
		{system.FailedStatus, system.FailedStatus, false},
		{system.FailedStatus, system.SucceedStatus, false},
		{system.FailedStatus, system.PausedStatus, false},
		{system.FailedStatus, system.EndStatus, true},

		{system.SucceedStatus, system.ReadyStatus, false},
		{system.SucceedStatus, system.RunningStatus, false},
		{system.SucceedStatus, system.FailedStatus, false},
		{system.SucceedStatus, system.SucceedStatus, false},
		{system.SucceedStatus, system.PausedStatus, false},
		{system.SucceedStatus, system.EndStatus, true},

		{system.PausedStatus, system.ReadyStatus, true},
		{system.PausedStatus, system.RunningStatus, false},
		{system.PausedStatus, system.FailedStatus, false},
		{system.PausedStatus, system.SucceedStatus, false},
		{system.PausedStatus, system.PausedStatus, false},
		{system.PausedStatus, system.EndStatus, true},

		{system.EndStatus, system.ReadyStatus, false},
		{system.EndStatus, system.RunningStatus, false},
		{system.EndStatus, system.FailedStatus, false},
		{system.EndStatus, system.SucceedStatus, false},
		{system.EndStatus, system.PausedStatus, false},
		{system.EndStatus, system.EndStatus, false},
	}

	for _, d := range data {
		if !c.Check(ValidTransitionJobState(d.from, d.to), C.Equals, d.valid) {
			c.Logf("Transition %s to %s failed (%v)\n", d.from, d.to, d.valid)
		}
	}
}

func (*testWrap) TestGetEnhancedLocalePackages(c *C.C) {
	if !system.QueryPackageInstalled("deepin-desktop-base") {
		c.Skip("deepin-desktop-base not installed")
		return
	}
	lang := "zh_CN.UTF-8"

	positive := []string{"firefox-dde", "libreoffice", "thunderbird", "gimp", "chromium-browser"}
	negative := []string{"vim", "lastore-daemon"}

	for _, p := range positive {
		d := pkg_recommend.GetEnhancedLocalePackages(lang, p)
		c.Check(len(d), C.Not(C.Equals), 0)
	}
	for _, p := range negative {
		d := pkg_recommend.GetEnhancedLocalePackages(lang, p)
		c.Check(len(d), C.Equals, 0)
	}
}

func (*testWrap) TestGuestJobType(c *C.C) {
	list := NewJobQueue("test", 100)
	j := NewJob(true, system.DistUpgradeJobType, []string{}, system.DistUpgradeJobType, "test")
	list.Add(j)
	list.Add(j)
	c.Check(list.Jobs.guest(system.DistUpgradeJobType, nil), C.Equals, system.DistUpgradeJobType)
	c.Check(list.Find(system.DistUpgradeJobType), C.Equals, j)

	j2 := NewJob(false, system.DistUpgradeJobType, []string{}, system.DistUpgradeJobType, "test")
	list2 := NewJobQueue("test", 100)
	list2.Add(j2)
	c.Check(list2.Find(system.DistUpgradeJobType), C.Not(C.Equals), j2)

}
