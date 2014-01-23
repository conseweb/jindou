// Copyright 2013 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package app

import (
	"github.com/xbee/jindou/quota"
	"labix.org/v2/mgo/bson"
	"launchpad.net/gocheck"
	"runtime"
	"sync"
)

func (s *S) TestReserveUnits(c *gocheck.C) {
	app := App{
		Name:  "together",
		Quota: quota.Quota{Limit: 7},
	}
	s.conn.Apps().Insert(app)
	defer s.conn.Apps().Remove(bson.M{"name": app.Name})
	err := reserveUnits(&app, 6)
	c.Assert(err, gocheck.IsNil)
	err = app.Get()
	c.Assert(err, gocheck.IsNil)
	c.Assert(app.Quota.InUse, gocheck.Equals, 6)
}

func (s *S) TestReserveUnitsAppNotFound(c *gocheck.C) {
	app := App{
		Name:  "together",
		Quota: quota.Quota{Limit: 7},
	}
	err := reserveUnits(&app, 6)
	c.Assert(err, gocheck.Equals, ErrAppNotFound)
}

func (s *S) TestReserveUnitsQuotaExceeded(c *gocheck.C) {
	app := App{
		Name:  "together",
		Quota: quota.Quota{Limit: 7},
	}
	s.conn.Apps().Insert(app)
	defer s.conn.Apps().Remove(bson.M{"name": app.Name})
	err := reserveUnits(&app, 6)
	c.Assert(err, gocheck.IsNil)
	err = reserveUnits(&app, 2)
	c.Assert(err, gocheck.NotNil)
	e, ok := err.(*quota.QuotaExceededError)
	c.Assert(ok, gocheck.Equals, true)
	c.Assert(e.Requested, gocheck.Equals, uint(2))
	c.Assert(e.Available, gocheck.Equals, uint(1))
}

func (s *S) TestReserveUnitsUnlimitedQuota(c *gocheck.C) {
	app := App{Name: "together", Quota: quota.Unlimited}
	s.conn.Apps().Insert(app)
	defer s.conn.Apps().Remove(bson.M{"name": app.Name})
	err := reserveUnits(&app, 6)
	c.Assert(err, gocheck.IsNil)
	err = app.Get()
	c.Assert(err, gocheck.IsNil)
	c.Assert(app.Quota.InUse, gocheck.Equals, 6)
}

func (s *S) TestReserveUnitsIsAtomic(c *gocheck.C) {
	ncpu := runtime.NumCPU()
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(ncpu))
	app := App{
		Name:  "together",
		Quota: quota.Quota{Limit: 40},
	}
	s.conn.Apps().Insert(app)
	defer s.conn.Apps().Remove(bson.M{"name": app.Name})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reserveUnits(&app, 3)
		}()
	}
	wg.Wait()
	err := app.Get()
	c.Assert(err, gocheck.IsNil)
	c.Assert(app.Quota.InUse, gocheck.Equals, 39)
}

func (s *S) TestReleaseUnits(c *gocheck.C) {
	app := App{
		Name:  "together",
		Quota: quota.Quota{Limit: 7, InUse: 7},
	}
	s.conn.Apps().Insert(app)
	defer s.conn.Apps().Remove(bson.M{"name": app.Name})
	err := releaseUnits(&app, 6)
	c.Assert(err, gocheck.IsNil)
	err = app.Get()
	c.Assert(err, gocheck.IsNil)
	c.Assert(app.Quota.InUse, gocheck.Equals, 1)
}

func (s *S) TestReleaseUnreservedUnits(c *gocheck.C) {
	app := App{
		Name:  "together",
		Quota: quota.Quota{Limit: 7, InUse: 7},
	}
	s.conn.Apps().Insert(app)
	defer s.conn.Apps().Remove(bson.M{"name": app.Name})
	err := releaseUnits(&app, 8)
	c.Assert(err, gocheck.NotNil)
	c.Assert(err.Error(), gocheck.Equals, "Not enough reserved units")
}

func (s *S) TestReleaseUnitsIsAtomic(c *gocheck.C) {
	ncpu := runtime.NumCPU()
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(ncpu))
	app := App{
		Name:  "together",
		Quota: quota.Quota{Limit: 40, InUse: 40},
	}
	s.conn.Apps().Insert(app)
	defer s.conn.Apps().Remove(bson.M{"name": app.Name})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			releaseUnits(&app, 3)
		}()
	}
	wg.Wait()
	err := app.Get()
	c.Assert(err, gocheck.IsNil)
	c.Assert(app.Quota.InUse, gocheck.Equals, 1)
}

func (s *S) TestReleaseUnitsAppNotFound(c *gocheck.C) {
	app := App{
		Name:  "together",
		Quota: quota.Quota{Limit: 7, InUse: 7},
	}
	err := releaseUnits(&app, 6)
	c.Assert(err, gocheck.Equals, ErrAppNotFound)
}
