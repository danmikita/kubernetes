/*
Copyright 2016 The Kubernetes Authors.

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

// Package policybased implements a standard storage for ClusterRole that prevents privilege escalation.
package policybased

import (
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/errors"
	"k8s.io/kubernetes/pkg/api/rest"
	"k8s.io/kubernetes/pkg/apis/rbac"
	"k8s.io/kubernetes/pkg/apis/rbac/validation"
	"k8s.io/kubernetes/pkg/auth/user"
	"k8s.io/kubernetes/pkg/runtime"
)

var groupResource = rbac.Resource("clusterroles")

type Storage struct {
	rest.StandardStorage

	ruleResolver validation.AuthorizationRuleResolver

	// user which skips privilege escalation checks
	superUser string
}

func NewStorage(s rest.StandardStorage, ruleResolver validation.AuthorizationRuleResolver, superUser string) *Storage {
	return &Storage{s, ruleResolver, superUser}
}

func (s *Storage) Create(ctx api.Context, obj runtime.Object) (runtime.Object, error) {
	if u, ok := api.UserFrom(ctx); ok {
		if s.superUser != "" && u.GetName() == s.superUser {
			return s.StandardStorage.Create(ctx, obj)
		}

		// system:masters is special because the API server uses it for privileged loopback connections
		// therefore we know that a member of system:masters can always do anything
		for _, group := range u.GetGroups() {
			if group == user.SystemPrivilegedGroup {
				return s.StandardStorage.Create(ctx, obj)
			}
		}
	}

	clusterRole := obj.(*rbac.ClusterRole)
	rules := clusterRole.Rules
	if err := validation.ConfirmNoEscalation(ctx, s.ruleResolver, rules); err != nil {
		return nil, errors.NewForbidden(groupResource, clusterRole.Name, err)
	}
	return s.StandardStorage.Create(ctx, obj)
}

func (s *Storage) Update(ctx api.Context, name string, obj rest.UpdatedObjectInfo) (runtime.Object, bool, error) {
	if user, ok := api.UserFrom(ctx); ok {
		if s.superUser != "" && user.GetName() == s.superUser {
			return s.StandardStorage.Update(ctx, name, obj)
		}
	}

	nonEscalatingInfo := wrapUpdatedObjectInfo(obj, func(ctx api.Context, obj runtime.Object, oldObj runtime.Object) (runtime.Object, error) {
		clusterRole := obj.(*rbac.ClusterRole)

		rules := clusterRole.Rules
		if err := validation.ConfirmNoEscalation(ctx, s.ruleResolver, rules); err != nil {
			return nil, errors.NewForbidden(groupResource, clusterRole.Name, err)
		}
		return obj, nil
	})

	return s.StandardStorage.Update(ctx, name, nonEscalatingInfo)
}

// TODO(ericchiang): This logic is copied from #26240. Replace with once that PR is merged into master.
type wrappedUpdatedObjectInfo struct {
	objInfo rest.UpdatedObjectInfo

	transformFunc rest.TransformFunc
}

func wrapUpdatedObjectInfo(objInfo rest.UpdatedObjectInfo, transformFunc rest.TransformFunc) rest.UpdatedObjectInfo {
	return &wrappedUpdatedObjectInfo{objInfo, transformFunc}
}

func (i *wrappedUpdatedObjectInfo) Preconditions() *api.Preconditions {
	return i.objInfo.Preconditions()
}

func (i *wrappedUpdatedObjectInfo) UpdatedObject(ctx api.Context, oldObj runtime.Object) (runtime.Object, error) {
	obj, err := i.objInfo.UpdatedObject(ctx, oldObj)
	if err != nil {
		return obj, err
	}
	return i.transformFunc(ctx, obj, oldObj)
}
