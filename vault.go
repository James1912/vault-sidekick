/*
Copyright 2015 Home Office All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"fmt"
	"time"

	"github.com/golang/glog"
	"github.com/hashicorp/vault/api"
	"crypto/tls"
"net/http"
)

// a channel to send resource
type resourceChannel chan *vaultResource

// vaultService ... is the main interface into the vault API - placing into a structure
// allows one to easily mock it and two to simplify the interface for us
type vaultService struct {
	// the vault client
	client *api.Client
	// the vault config
	config *api.Config
	// a channel to inform of a new resource to processor
	resourceChannel chan *watchedResource
}

type vaultResourceEvent struct {
	// the resource this relates to
	resource *vaultResource
	// the secret associated
	secret map[string]interface{}
}

// newVaultService ... creates a new implementation to speak to vault and retrieve the resources
//	url			: the url of the vault service
func newVaultService(url string) (*vaultService, error) {
	var err error
	glog.Infof("creating a new vault client: %s", url)

	// step: create the config for client
	service := new(vaultService)
	service.config = api.DefaultConfig()
	service.config.Address = url

	// step: skip the cert verification if requested
	if options.skipTLSVerify {
		service.config.HttpClient.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	// step: create the service processor channels
	service.resourceChannel = make(chan *watchedResource, 20)

	// step: create the actual client
	service.client, err = api.NewClient(service.config)
	if err != nil {
		return nil, err
	}

	// step: are we using a token? or do we need to authenticate and grab a token
	if options.vaultToken == "" {
		options.vaultToken, err = service.authenticate(options.vaultAuthOptions)
		if err != nil {
			return nil, err
		}
	}

	// step: set the token for the client
	service.client.SetToken(options.vaultToken)

	// step: start the service processor off
	service.vaultServiceProcessor()

	return service, nil
}

// vaultServiceProcessor ... is the background routine responsible for retrieving the resources, renewing when required and
// informing those who are watching the resource that something has changed
func (r vaultService) vaultServiceProcessor() {
	go func() {
		// a list of resource being watched
		var items []*watchedResource

		// the channel to receive renewal notifications on
		renewChannel := make(chan *watchedResource, 10)
		retrieveChannel := make(chan *watchedResource, 10)
		revokeChannel := make(chan string, 10)
		statsChannel := time.NewTicker(options.statsInterval)

		for {
			select {
			// A new resource is being added to the service processor;
			//  - we retrieve the resource from vault
			//  - if we error attempting to retrieve the secret, we background and reschedule an attempt to add it
			//  - if ok, we grab the lease it and lease time, we setup a notification on renewal
			case x := <-r.resourceChannel:
				glog.Infof("adding a resource into the service processor, resource: %s", x.resource)
				// step: add to the list of resources
				items = append(items, x)
				// step: push into the retrieval channel
				retrieveChannel <- x

			case x := <-retrieveChannel:
				// step: save the current lease if we have one
				leaseID := ""
				if x.secret != nil && x.secret.LeaseID != "" {
					leaseID = x.secret.LeaseID
					glog.V(10).Infof("resource: %s has a previous lease: %s", x.resource, leaseID)
				}

				// step: retrieve the resource from vault
				err := r.get(x)
				if err != nil {
					glog.Errorf("failed to retrieve the resource: %s from vault, error: %s", x.resource, err)
					// reschedule the attempt for later
					r.reschedule(x, retrieveChannel, 3, 10)

					break
				}

				glog.Infof("succesfully retrieved resournce: %s, leaseID: %s", x.resource, x.secret.LeaseID)

				// step: if we had a previous lease and the option is to revoke, lets throw into the revoke channel
				if leaseID != "" && x.resource.revoked {
					revokeChannel <- leaseID
				}

				// step: setup a timer for renewal
				x.notifyOnRenewal(renewChannel)

				// step: update the upstream consumers
				r.upstream(x)

			// A watched resource is coming up for renewal
			// 	- we attempt to renew the resource from vault
			//	- if we encounter an error, we reschedule the attempt for the future
			//	- if we're ok, we update the watchedResource and we send a notification of the change upstream
			case x := <-renewChannel:

				glog.V(4).Infof("resource: %s, lease: %s up for renewal, renewable: %t, revoked: %t", x.resource,
					x.secret.LeaseID, x.resource.renewable, x.resource.revoked)

				// step: we need to check if the lease has expired?
				if time.Now().Before(x.leaseExpireTime) {
					glog.V(3).Infof("the lease on resource: %s has expired, we need to get a new lease", x.resource)
					// push into the retrieval channel and break
					retrieveChannel <- x
					break
				}

				// step: are we renewing the resource?
				if x.resource.renewable {
					// step: is the underlining resource even renewable? - otherwise we can just grab a new lease
					if !x.secret.Renewable {
						glog.V(10).Infof("the resource: %s is not renewable, retrieving a new lease instead", x.resource)
						retrieveChannel <- x
						break
					}

					// step: lets renew the resource
					err := r.renew(x)
					if err != nil {
						glog.Errorf("failed to renew the resounce: %s for renewal, error: %s", x.resource, err)
						// reschedule the attempt for later
						r.reschedule(x, renewChannel, 3, 10)
						break
					}
				}

				// step: the option for this resource is not to renew the secret but regenerate a new secret
				if !x.resource.renewable {
					glog.V(4).Infof("resource: %s flagged as not renewable, shifting to regenerating the resource", x.resource)
					retrieveChannel <- x
					break
				}

				// step: setup a timer for renewal
				x.notifyOnRenewal(renewChannel)

				// step: update any listener upstream
				r.upstream(x)

			case lease := <-revokeChannel:

				err := r.revoke(lease)
				if err != nil {
					glog.Errorf("failed to revoke the lease: %s, error: %s", lease, err)
				}

			// The statistics timer has gone off; we iterate the watched items and
			case <-statsChannel.C:
				glog.V(3).Infof("stats: %d resources being watched", len(items))
				for _, item := range items {
					glog.V(3).Infof("resourse: %s, lease id: %s, renewal in: %s seconds, expiration: %s",
						item.resource, item.secret.LeaseID, item.renewalTime, item.leaseExpireTime)
				}
			}
		}
	}()
}

// authenticate ... we need to authenticate to teh vault to grab a toke
//	auth		: a map containing the options required for authentication
func (r vaultService) authenticate(auth map[string]string) (string, error) {
	var secret *api.Secret
	var err error

	plugin, _ := auth["method"]
	switch plugin {
	case "userpass":
		// step: get the options for this plugin
		username, _ := auth["username"]
		password, _ := auth["password"]
		secret, err = newUserPass(r.client).create(username, password)

	default:
		return "", fmt.Errorf("unsupported authentication plugin: %s", plugin)
	}
	// step: was there an error?
	if err != nil {
		return "", err
	}

	// step: do we have auth information
	if secret.Auth == nil {
		return "", fmt.Errorf("invalid authentication response, no auth response")
	}

	// step: return the client token
	return secret.Auth.ClientToken, nil
}

// reschedule ... reschedules an event back into a channel after n seconds
//	rn			: a pointer to the watched resource you wish to reschedule
//	ch			: the channel the resource should be placed into
//	min			: the minimum amount of time i'm willing to wait
//	max			: the maximum amount of time i'm willing to wait
func (r vaultService) reschedule(rn *watchedResource, ch chan *watchedResource, min, max int) {
	go func(x *watchedResource) {
		glog.V(3).Infof("rescheduling the resource: %s, channel: %v", rn.resource, ch)
		<-randomWait(min, max)
		ch <- x
	}(rn)
}

// upstream ... the resource has changed thus we notify the upstream listener
//	item		: the item which has changed
func (r vaultService) upstream(item *watchedResource) {
	// step: chunk this into a go-routine not to block us
	go func() {
		glog.V(6).Infof("sending the event for resource: %s upstream to listener: %v", item.resource, item.listener)
		item.listener <- vaultResourceEvent{
			resource: item.resource,
			secret:   item.secret.Data,
		}
	}()
}

// renew ... attempts to renew the lease on a resource
// 	rn			: the resource we wish to renew the lease on
func (r vaultService) renew(rn *watchedResource) error {
	// step: extend the lease on a resource
	glog.V(4).Infof("attempting to renew the lease: %s on resource: %s", rn.secret.LeaseID, rn.resource)
	// step: check the resource is renewable
	if !rn.secret.Renewable {
		return fmt.Errorf("the resource: %s is not renewable", rn.resource)
	}

	secret, err := r.client.Sys().Renew(rn.secret.LeaseID, 0)
	if err != nil {
		glog.Errorf("unable to renew the lease on resource: %s", rn.resource)
		return err
	}

	// step: update the resource
	rn.lastUpdated = time.Now()
	rn.leaseExpireTime = rn.lastUpdated.Add(time.Duration(secret.LeaseDuration))

	glog.V(3).Infof("renewed resource: %s, leaseId: %s, lease_time: %s, expiration: %s",
		rn.resource, rn.secret.LeaseID, rn.secret.LeaseID, rn.leaseExpireTime)

	return nil
}

// revoke ... attempt to revoke the lease of a resource
//	lease			: the lease lease which was given when you got it
func (r vaultService) revoke(lease string) error {
	glog.V(3).Infof("attemping to revoking the lease: %s", lease)

	err := r.client.Sys().Revoke(lease)
	if err != nil {
		return err
	}
	glog.V(3).Infof("successfully revoked the leaseId: %s", lease)

	return nil
}

// get ... retrieve a secret from the vault
func (r vaultService) get(rn *watchedResource) (err error) {
	var secret *api.Secret
	glog.V(5).Infof("attempting to retrieve the resource: %s from vault", rn.resource)

	switch rn.resource.resource {
	case "pki":
		secret, err = r.client.Logical().Write(fmt.Sprintf("%s/issue/%s", rn.resource.resource, rn.resource.name),
			map[string]interface{}{
				"common_name": rn.resource.options[OptionCommonName],
			})
	case "aws":
		secret, err = r.client.Logical().Read(fmt.Sprintf("%s/creds/%s", rn.resource.resource, rn.resource.name))
	case "mysql":
		secret, err = r.client.Logical().Read(fmt.Sprintf("%s/creds/%s", rn.resource.resource, rn.resource.name))
	case "secret":
		secret, err = r.client.Logical().Read(fmt.Sprintf("%s/%s", rn.resource.resource, rn.resource.name))
	}
	// step: return on error
	if err != nil {
		return err
	}
	if secret == nil && err != nil {
		return fmt.Errorf("the resource does not exist")
	}

	if secret == nil {
		return fmt.Errorf("unable to retrieve the secret")
	}

	// step: update the watched resource
	rn.lastUpdated = time.Now()
	rn.secret = secret
	rn.leaseExpireTime = rn.lastUpdated.Add(time.Duration(secret.LeaseDuration))

	glog.V(3).Infof("retrieved resource: %s, leaseId: %s, lease_time: %s",
		rn.resource, rn.secret.LeaseID, time.Duration(rn.secret.LeaseDuration)*time.Second)

	return err
}

// watch ... add a watch on a resource and inform, renew which required and inform us when
// the resource is ready
func (r *vaultService) watch(rn *vaultResource, ch chan vaultResourceEvent) {
	glog.V(6).Infof("adding the resource: %s, listener: %v to service processor", rn, ch)

	r.resourceChannel <- &watchedResource{
		resource: rn,
		listener: ch,
	}
}
