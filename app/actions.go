// Copyright 2013 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package app

import (
	"errors"
	"fmt"
	"github.com/globocom/config"
	"github.com/globocom/go-gandalfclient"
	"github.com/xbee/jindou/action"
	"github.com/xbee/jindou/app/bind"
	"github.com/xbee/jindou/auth"
	"github.com/xbee/jindou/db"
	"github.com/xbee/jindou/log"
	"github.com/xbee/jindou/provision"
	"github.com/xbee/jindou/queue"
	"github.com/xbee/jindou/quota"
	"github.com/xbee/jindou/repository"
	"io"
	"labix.org/v2/mgo"
	"labix.org/v2/mgo/bson"
	"launchpad.net/goamz/aws"
	"launchpad.net/goamz/iam"
	"strconv"
)

var (
	ErrAppAlreadyExists = errors.New("there is already an app with this name.")
	ErrAppNotFound      = errors.New("App not found")
)

// reserveUserApp reserves the app for the user, only if the user has a quota
// of apps. If the user does not have a quota, meaning that it's unlimited,
// reserveUserApp.Forward just return nil.
var reserveUserApp = action.Action{
	Name: "reserve-user-app",
	Forward: func(ctx action.FWContext) (action.Result, error) {
		var app App
		switch ctx.Params[0].(type) {
		case App:
			app = ctx.Params[0].(App)
		case *App:
			app = *ctx.Params[0].(*App)
		default:
			return nil, errors.New("First parameter must be App or *App.")
		}
		var user auth.User
		switch ctx.Params[1].(type) {
		case auth.User:
			user = ctx.Params[1].(auth.User)
		case *auth.User:
			user = *ctx.Params[1].(*auth.User)
		default:
			return nil, errors.New("Third parameter must be auth.User or *auth.User.")
		}
		usr, err := auth.GetUserByEmail(user.Email)
		if err != nil {
			return nil, err
		}
		if err := auth.ReserveApp(usr); err != nil {
			return nil, err
		}
		return map[string]string{"app": app.Name, "user": user.Email}, nil
	},
	Backward: func(ctx action.BWContext) {
		m := ctx.FWResult.(map[string]string)
		if user, err := auth.GetUserByEmail(m["user"]); err == nil {
			auth.ReleaseApp(user)
		}
	},
	MinParams: 2,
}

// insertApp is an action that inserts an app in the database in Forward and
// removes it in the Backward.
//
// The first argument in the context must be an App or a pointer to an App.
var insertApp = action.Action{
	Name: "insert-app",
	Forward: func(ctx action.FWContext) (action.Result, error) {
		var app App
		switch ctx.Params[0].(type) {
		case App:
			app = ctx.Params[0].(App)
		case *App:
			app = *ctx.Params[0].(*App)
		default:
			return nil, errors.New("First parameter must be App or *App.")
		}
		conn, err := db.Conn()
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		app.Quota = quota.Unlimited
		if limit, err := config.GetInt("quota:units-per-app"); err == nil {
			app.Quota.Limit = limit
		}
		app.Units = append(app.Units, Unit{})
		err = conn.Apps().Insert(app)
		if mgo.IsDup(err) {
			return nil, ErrAppAlreadyExists
		}
		return &app, err
	},
	Backward: func(ctx action.BWContext) {
		app := ctx.FWResult.(*App)
		conn, err := db.Conn()
		if err != nil {
			log.Errorf("Could not connect to the database: %s", err)
			return
		}
		defer conn.Close()
		conn.Apps().Remove(bson.M{"name": app.Name})
	},
	MinParams: 1,
}

// createIAMUserAction creates a user in IAM. It requires that the first
// parameter is the a pointer to an App instance.
var createIAMUserAction = action.Action{
	Name: "create-iam-user",
	Forward: func(ctx action.FWContext) (action.Result, error) {
		app := ctx.Previous.(*App)
		return createIAMUser(app.Name)
	},
	Backward: func(ctx action.BWContext) {
		user := ctx.FWResult.(*iam.User)
		getIAMEndpoint().DeleteUser(user.Name)
	},
	MinParams: 1,
}

// createIAMAccessKeyAction creates an access key in IAM. It uses the result
// returned by createIAMUserAction, so it must come after this action.
var createIAMAccessKeyAction = action.Action{
	Name: "create-iam-access-key",
	Forward: func(ctx action.FWContext) (action.Result, error) {
		user := ctx.Previous.(*iam.User)
		return createIAMAccessKey(user)
	},
	Backward: func(ctx action.BWContext) {
		key := ctx.FWResult.(*iam.AccessKey)
		getIAMEndpoint().DeleteAccessKey(key.Id, key.UserName)
	},
	MinParams: 1,
}

// createBucketAction creates a bucket in S3. It uses the result of
// createIAMAccessKeyAction for managing permission, and the app given as
// parameter to generate the name of the bucket. It must run after
// createIAMAccessKey.
var createBucketAction = action.Action{
	Name: "create-bucket",
	Forward: func(ctx action.FWContext) (action.Result, error) {
		app := ctx.Params[0].(*App)
		key := ctx.Previous.(*iam.AccessKey)
		bucket, err := putBucket(app.Name)
		if err != nil {
			return nil, err
		}
		env := s3Env{
			Auth: aws.Auth{
				AccessKey: key.Id,
				SecretKey: key.Secret,
			},
			bucket:             bucket.Name,
			endpoint:           bucket.S3Endpoint,
			locationConstraint: bucket.S3LocationConstraint,
		}
		return &env, nil
	},
	Backward: func(ctx action.BWContext) {
		env := ctx.FWResult.(*s3Env)
		getS3Endpoint().Bucket(env.bucket).DelBucket()
	},
	MinParams: 1,
}

// createUserPolicyAction creates a new UserPolicy in IAM. It requires a
// pointer to an App instance as the first parameter, and the previous result
// to be a *s3Env (it should be used after createBucketAction).
var createUserPolicyAction = action.Action{
	Name: "create-user-policy",
	Forward: func(ctx action.FWContext) (action.Result, error) {
		app := ctx.Params[0].(*App)
		env := ctx.Previous.(*s3Env)
		_, err := createIAMUserPolicy(&iam.User{Name: app.Name}, app.Name, env.bucket)
		if err != nil {
			return nil, err
		}
		return ctx.Previous, nil
	},
	Backward: func(ctx action.BWContext) {
		app := ctx.Params[0].(*App)
		policyName := fmt.Sprintf("app-%s-bucket", app.Name)
		getIAMEndpoint().DeleteUserPolicy(app.Name, policyName)
	},
	MinParams: 1,
}

// exportEnvironmentsAction exports tsuru's default environment variables in a
// new app. It requires a pointer to an App instance as the first parameter,
// and the previous result to be a *s3Env (it should be used after
// createUserPolicyAction or createBucketAction).
var exportEnvironmentsAction = action.Action{
	Name: "export-environments",
	Forward: func(ctx action.FWContext) (action.Result, error) {
		app := ctx.Params[0].(*App)
		err := app.Get()
		if err != nil {
			return nil, err
		}
		t, err := auth.CreateApplicationToken(app.Name)
		if err != nil {
			return nil, err
		}
		host, _ := config.GetString("host")
		envVars := []bind.EnvVar{
			{Name: "TSURU_APPNAME", Value: app.Name},
			{Name: "TSURU_HOST", Value: host},
			{Name: "TSURU_APP_TOKEN", Value: t.Token},
		}
		env, ok := ctx.Previous.(*s3Env)
		if ok {
			variables := map[string]string{
				"ENDPOINT":           env.endpoint,
				"LOCATIONCONSTRAINT": strconv.FormatBool(env.locationConstraint),
				"ACCESS_KEY_ID":      env.AccessKey,
				"SECRET_KEY":         env.SecretKey,
				"BUCKET":             env.bucket,
			}
			for name, value := range variables {
				envVars = append(envVars, bind.EnvVar{
					Name:         fmt.Sprintf("TSURU_S3_%s", name),
					Value:        value,
					InstanceName: s3InstanceName,
				})
			}
		}
		err = app.setEnvsToApp(envVars, false, true)
		if err != nil {
			return nil, err
		}
		return ctx.Previous, nil
	},
	Backward: func(ctx action.BWContext) {
		app := ctx.Params[0].(*App)
		auth.DeleteToken(app.Env["TSURU_APP_TOKEN"].Value)
		if app.Get() == nil {
			s3Env := app.InstanceEnv(s3InstanceName)
			vars := make([]string, len(s3Env)+3)
			i := 0
			for k := range s3Env {
				vars[i] = k
				i++
			}
			vars[i] = "TSURU_HOST"
			vars[i+1] = "TSURU_APPNAME"
			vars[i+2] = "TSURU_APP_TOKEN"
			app.UnsetEnvs(vars, false)
		}
	},
	MinParams: 1,
}

// createRepository creates a repository for the app in Gandalf.
var createRepository = action.Action{
	Name: "create-repository",
	Forward: func(ctx action.FWContext) (action.Result, error) {
		var app App
		switch ctx.Params[0].(type) {
		case App:
			app = ctx.Params[0].(App)
		case *App:
			app = *ctx.Params[0].(*App)
		default:
			return nil, errors.New("First parameter must be App or *App.")
		}
		gURL := repository.ServerURL()
		var users []string
		for _, t := range app.GetTeams() {
			users = append(users, t.Users...)
		}
		c := gandalf.Client{Endpoint: gURL}
		_, err := c.NewRepository(app.Name, users, false)
		return &app, err
	},
	Backward: func(ctx action.BWContext) {
		app := ctx.FWResult.(*App)
		app.Get()
		gURL := repository.ServerURL()
		c := gandalf.Client{Endpoint: gURL}
		c.RemoveRepository(app.Name)
	},
	MinParams: 1,
}

// provisionApp provisions the app in the provisioner. It takes two arguments:
// the app, and the number of units to create (an unsigned integer).
var provisionApp = action.Action{
	Name: "provision-app",
	Forward: func(ctx action.FWContext) (action.Result, error) {
		var app App
		switch ctx.Params[0].(type) {
		case App:
			app = ctx.Params[0].(App)
		case *App:
			app = *ctx.Params[0].(*App)
		default:
			return nil, errors.New("First parameter must be App or *App.")
		}
		err := Provisioner.Provision(&app)
		if err != nil {
			return nil, err
		}
		return &app, nil
	},
	Backward: func(ctx action.BWContext) {
		app := ctx.FWResult.(*App)
		Provisioner.Destroy(app)
	},
	MinParams: 1,
}

var reserveUnitsToAdd = action.Action{
	Name: "reserve-units-to-add",
	Forward: func(ctx action.FWContext) (action.Result, error) {
		var app App
		switch ctx.Params[0].(type) {
		case App:
			app = ctx.Params[0].(App)
		case *App:
			app = *ctx.Params[0].(*App)
		default:
			return nil, errors.New("First parameter must be App or *App.")
		}
		var n int
		switch ctx.Params[1].(type) {
		case int:
			n = ctx.Params[1].(int)
		case uint:
			n = int(ctx.Params[1].(uint))
		default:
			return nil, errors.New("Second parameter must be int or uint.")
		}
		conn, err := db.Conn()
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		err = app.Get()
		if err != nil {
			return nil, ErrAppNotFound
		}
		err = reserveUnits(&app, n)
		if err != nil {
			return nil, err
		}
		return n, nil
	},
	Backward: func(ctx action.BWContext) {
		var app App
		switch ctx.Params[0].(type) {
		case App:
			app = ctx.Params[0].(App)
		case *App:
			app = *ctx.Params[0].(*App)
		}
		qty := ctx.FWResult.(int)
		err := releaseUnits(&app, qty)
		if err != nil {
			log.Errorf("Failed to rollback reserveUnitsToAdd: %s", err)
		}
	},
	MinParams: 2,
}

type addUnitsActionResult struct {
	units []provision.Unit
}

var provisionAddUnits = action.Action{
	Name: "provision-add-units",
	Forward: func(ctx action.FWContext) (action.Result, error) {
		var app App
		switch ctx.Params[0].(type) {
		case App:
			app = ctx.Params[0].(App)
		case *App:
			app = *ctx.Params[0].(*App)
		default:
			return nil, errors.New("First parameter must be App or *App.")
		}
		n := ctx.Previous.(int)
		var result addUnitsActionResult
		units, err := Provisioner.AddUnits(&app, uint(n))
		if err != nil {
			return nil, err
		}
		result.units = units
		return &result, nil
	},
	Backward: func(ctx action.BWContext) {
		var app App
		switch ctx.Params[0].(type) {
		case App:
			app = ctx.Params[0].(App)
		case *App:
			app = *ctx.Params[0].(*App)
		}
		fwResult := ctx.FWResult.(*addUnitsActionResult)
		for _, unit := range fwResult.units {
			Provisioner.RemoveUnit(&app, unit.Name)
		}
	},
	MinParams: 1,
}

var saveNewUnitsInDatabase = action.Action{
	Name: "save-new-units-in-database",
	Forward: func(ctx action.FWContext) (action.Result, error) {
		var app App
		switch ctx.Params[0].(type) {
		case App:
			app = ctx.Params[0].(App)
		case *App:
			app = *ctx.Params[0].(*App)
		default:
			return nil, errors.New("First parameter must be App or *App.")
		}
		prev := ctx.Previous.(*addUnitsActionResult)
		conn, err := db.Conn()
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		err = app.Get()
		if err != nil {
			return nil, ErrAppNotFound
		}
		messages := make([]queue.Message, len(prev.units)*2)
		mCount := 0
		for _, unit := range prev.units {
			unit := Unit{
				Name:       unit.Name,
				Type:       unit.Type,
				Ip:         unit.Ip,
				Machine:    unit.Machine,
				State:      provision.StatusBuilding.String(),
				InstanceId: unit.InstanceId,
			}
			app.AddUnit(&unit)
			messages[mCount] = queue.Message{Action: RegenerateApprcAndStart, Args: []string{app.Name, unit.Name}}
			messages[mCount+1] = queue.Message{Action: BindService, Args: []string{app.Name, unit.Name}}
			mCount += 2
		}
		err = conn.Apps().Update(
			bson.M{"name": app.Name},
			bson.M{"$set": bson.M{"units": app.Units}},
		)
		if err != nil {
			return nil, err
		}
		go Enqueue(messages...)
		return nil, nil
	},
	MinParams: 1,
}

// ProvisionerDeploy is an actions that call the Provisioner.Deploy.
var ProvisionerDeploy = action.Action{
	Name: "provisioner-deploy",
	Forward: func(ctx action.FWContext) (action.Result, error) {
		app, ok := ctx.Params[0].(*App)
		if !ok {
			return nil, errors.New("First parameter must be a *App.")
		}
		version, ok := ctx.Params[1].(string)
		if !ok {
			return nil, errors.New("Second parameter must be a string.")
		}
		logWriter, ok := ctx.Params[2].(io.Writer)
		if !ok {
			return nil, errors.New("Third parameter must be a io.Writer.")
		}
		err := Provisioner.Deploy(app, version, logWriter)
		return nil, err
	},
	Backward: func(ctx action.BWContext) {
	},
	MinParams: 3,
}

// Increment is an actions that increments the deploy number.
var IncrementDeploy = action.Action{
	Name: "increment-deploy",
	Forward: func(ctx action.FWContext) (action.Result, error) {
		app, ok := ctx.Params[0].(*App)
		if !ok {
			return nil, errors.New("First parameter must be a *App.")
		}
		err := incrementDeploy(app)
		return nil, err
	},
	Backward: func(ctx action.BWContext) {
	},
	MinParams: 1,
}
