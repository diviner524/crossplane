/*
Copyright 2020 The Crossplane Authors.

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

package composite

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"
	"github.com/crossplane/crossplane-runtime/pkg/resource/fake"
	"github.com/crossplane/crossplane-runtime/pkg/test"

	v1 "github.com/crossplane/crossplane/apis/apiextensions/v1"
	"github.com/crossplane/crossplane/internal/xcrd"
)

func TestPTCompose(t *testing.T) {
	errBoom := errors.New("boom")
	details := managed.ConnectionDetails{"a": []byte("b")}

	type params struct {
		kube client.Client
		o    []PTComposerOption
	}
	type args struct {
		ctx context.Context
		xr  resource.Composite
		req CompositionRequest
	}
	type want struct {
		res CompositionResult
		err error
	}

	cases := map[string]struct {
		reason string
		params params
		args   args
		want   want
	}{
		"ComposedTemplatesError": {
			reason: "We should return any error encountered while inlining a composition's patchsets.",
			args: args{
				req: CompositionRequest{
					Revision: &v1.CompositionRevision{
						Spec: v1.CompositionRevisionSpec{
							Resources: []v1.ComposedTemplate{{
								Patches: []v1.Patch{{
									// This reference to a non-existent patchset
									// triggers the error.
									Type:         v1.PatchTypePatchSet,
									PatchSetName: pointer.String("nonexistent-patchset"),
								}},
							}},
						},
					},
				},
			},
			want: want{
				err: errors.Wrap(errors.Errorf(errFmtUndefinedPatchSet, "nonexistent-patchset"), errInline),
			},
		},
		"AssociateTemplatesError": {
			reason: "We should return any error encountered while associating Composition templates with composed resources.",
			params: params{
				o: []PTComposerOption{
					WithTemplateAssociator(CompositionTemplateAssociatorFn(func(ctx context.Context, c resource.Composite, ct []v1.ComposedTemplate) ([]TemplateAssociation, error) {
						return nil, errBoom
					})),
				},
			},
			args: args{
				req: CompositionRequest{
					Revision: &v1.CompositionRevision{},
				},
			},
			want: want{
				err: errors.Wrap(errBoom, errAssociate),
			},
		},
		// TODO(negz): Test handling of ApplyEnvironmentPatch errors.
		"RenderComposedError": {
			reason: "We should include any error encountered while rendering a composed resource as a warning, not as the returned error.",
			params: params{
				kube: &test.MockClient{
					MockUpdate: test.NewMockUpdateFn(nil),

					// Apply uses Get and Patch.
					MockGet:   test.NewMockGetFn(nil),
					MockPatch: test.NewMockPatchFn(nil),
				},
				o: []PTComposerOption{
					WithTemplateAssociator(CompositionTemplateAssociatorFn(func(ctx context.Context, c resource.Composite, ct []v1.ComposedTemplate) ([]TemplateAssociation, error) {
						tas := []TemplateAssociation{{
							Template: v1.ComposedTemplate{
								Name: pointer.String("cool-resource"),
							},
						}}
						return tas, nil
					})),
					WithComposedRenderer(RendererFn(func(ctx context.Context, cp resource.Composite, cd resource.Composed, t v1.ComposedTemplate, env *Environment) error {
						return errBoom
					})),
					WithCompositeRenderer(RendererFn(func(ctx context.Context, cp resource.Composite, cd resource.Composed, t v1.ComposedTemplate, env *Environment) error {
						return nil
					})),
					WithComposedConnectionDetailsExtractor(ConnectionDetailsExtractorFn(func(cd resource.Composed, conn managed.ConnectionDetails, cfg ...ConnectionDetailExtractConfig) (managed.ConnectionDetails, error) {
						return nil, nil
					})),
				},
			},
			args: args{
				xr: &fake.Composite{},
				req: CompositionRequest{
					Revision: &v1.CompositionRevision{},
				},
			},
			want: want{
				res: CompositionResult{
					Composed: []ComposedResource{{
						ResourceName: "cool-resource",
					}},
					ConnectionDetails: managed.ConnectionDetails{},
					Events: []event.Event{
						event.Warning(reasonCompose, errors.Wrapf(errBoom, errFmtResourceName, "cool-resource")),
					},
				},
			},
		},
		"UpdateCompositeError": {
			reason: "We should return any error encountered while updating our composite resource with references.",
			params: params{
				kube: &test.MockClient{
					MockUpdate: test.NewMockUpdateFn(errBoom),
				},
				o: []PTComposerOption{
					WithTemplateAssociator(CompositionTemplateAssociatorFn(func(ctx context.Context, c resource.Composite, ct []v1.ComposedTemplate) ([]TemplateAssociation, error) {
						tas := []TemplateAssociation{{
							Template: v1.ComposedTemplate{
								Name: pointer.String("cool-resource"),
							},
						}}
						return tas, nil
					})),
					WithComposedRenderer(RendererFn(func(ctx context.Context, cp resource.Composite, cd resource.Composed, t v1.ComposedTemplate, env *Environment) error {
						return nil
					})),
				},
			},
			args: args{
				xr: &fake.Composite{},
				req: CompositionRequest{
					Revision: &v1.CompositionRevision{},
				},
			},
			want: want{
				err: errors.Wrap(errBoom, errUpdate),
			},
		},
		"ApplyComposedError": {
			reason: "We should return any error encountered while applying a composed resource.",
			params: params{
				kube: &test.MockClient{
					MockUpdate: test.NewMockUpdateFn(nil),

					// Apply calls Get.
					MockGet: test.NewMockGetFn(errBoom),
				},
				o: []PTComposerOption{
					WithTemplateAssociator(CompositionTemplateAssociatorFn(func(ctx context.Context, c resource.Composite, ct []v1.ComposedTemplate) ([]TemplateAssociation, error) {
						tas := []TemplateAssociation{{
							Template: v1.ComposedTemplate{
								Name: pointer.String("cool-resource"),
							},
						}}
						return tas, nil
					})),
					WithComposedRenderer(RendererFn(func(ctx context.Context, cp resource.Composite, cd resource.Composed, t v1.ComposedTemplate, env *Environment) error {
						return nil
					})),
				},
			},
			args: args{
				xr: &fake.Composite{},
				req: CompositionRequest{
					Revision: &v1.CompositionRevision{},
				},
			},
			want: want{
				err: errors.Wrap(errors.Wrap(errBoom, "cannot get object"), errApply),
			},
		},
		"CompositeRenderError": {
			reason: "We should return any error encountered while rendering the Composite.",
			params: params{
				kube: &test.MockClient{
					MockUpdate: test.NewMockUpdateFn(nil),

					// Apply calls Get and Patch
					MockGet:   test.NewMockGetFn(nil),
					MockPatch: test.NewMockPatchFn(nil),
				},
				o: []PTComposerOption{
					WithTemplateAssociator(CompositionTemplateAssociatorFn(func(ctx context.Context, c resource.Composite, ct []v1.ComposedTemplate) ([]TemplateAssociation, error) {
						tas := []TemplateAssociation{{
							Template: v1.ComposedTemplate{
								Name: pointer.String("cool-resource"),
							},
						}}
						return tas, nil
					})),
					WithComposedRenderer(RendererFn(func(ctx context.Context, cp resource.Composite, cd resource.Composed, t v1.ComposedTemplate, env *Environment) error {
						return nil
					})),
					WithCompositeRenderer(RendererFn(func(ctx context.Context, cp resource.Composite, cd resource.Composed, t v1.ComposedTemplate, env *Environment) error {
						return errBoom
					})),
				},
			},
			args: args{
				xr: &fake.Composite{},
				req: CompositionRequest{
					Revision: &v1.CompositionRevision{},
				},
			},
			want: want{
				err: errors.Wrap(errBoom, errRenderCR),
			},
		},
		"FetchConnectionDetailsError": {
			reason: "We should return any error encountered while fetching a composed resource's connection details.",
			params: params{
				kube: &test.MockClient{
					MockUpdate: test.NewMockUpdateFn(nil),

					// Apply calls Get and Patch
					MockGet:   test.NewMockGetFn(nil),
					MockPatch: test.NewMockPatchFn(nil),
				},
				o: []PTComposerOption{
					WithTemplateAssociator(CompositionTemplateAssociatorFn(func(ctx context.Context, c resource.Composite, ct []v1.ComposedTemplate) ([]TemplateAssociation, error) {
						tas := []TemplateAssociation{{
							Template: v1.ComposedTemplate{
								Name: pointer.String("cool-resource"),
							},
						}}
						return tas, nil
					})),
					WithComposedRenderer(RendererFn(func(ctx context.Context, cp resource.Composite, cd resource.Composed, t v1.ComposedTemplate, env *Environment) error {
						return nil
					})),
					WithCompositeRenderer(RendererFn(func(ctx context.Context, cp resource.Composite, cd resource.Composed, t v1.ComposedTemplate, env *Environment) error {
						return nil
					})),
					WithComposedConnectionDetailsFetcher(ConnectionDetailsFetcherFn(func(ctx context.Context, o resource.ConnectionSecretOwner) (managed.ConnectionDetails, error) {
						return nil, errBoom
					})),
				},
			},
			args: args{
				xr: &fake.Composite{},
				req: CompositionRequest{
					Revision: &v1.CompositionRevision{},
				},
			},
			want: want{
				err: errors.Wrap(errBoom, errFetchDetails),
			},
		},
		"ExtractConnectionDetailsError": {
			reason: "We should return any error encountered while extracting a composed resource's connection details.",
			params: params{
				kube: &test.MockClient{
					MockUpdate: test.NewMockUpdateFn(nil),

					// Apply calls Get and Patch
					MockGet:   test.NewMockGetFn(nil),
					MockPatch: test.NewMockPatchFn(nil),
				},
				o: []PTComposerOption{
					WithTemplateAssociator(CompositionTemplateAssociatorFn(func(ctx context.Context, c resource.Composite, ct []v1.ComposedTemplate) ([]TemplateAssociation, error) {
						tas := []TemplateAssociation{{
							Template: v1.ComposedTemplate{
								Name: pointer.String("cool-resource"),
							},
						}}
						return tas, nil
					})),
					WithComposedRenderer(RendererFn(func(ctx context.Context, cp resource.Composite, cd resource.Composed, t v1.ComposedTemplate, env *Environment) error {
						return nil
					})),
					WithCompositeRenderer(RendererFn(func(ctx context.Context, cp resource.Composite, cd resource.Composed, t v1.ComposedTemplate, env *Environment) error {
						return nil
					})),
					WithComposedConnectionDetailsFetcher(ConnectionDetailsFetcherFn(func(ctx context.Context, o resource.ConnectionSecretOwner) (managed.ConnectionDetails, error) {
						return nil, nil
					})),
					WithComposedConnectionDetailsExtractor(ConnectionDetailsExtractorFn(func(cd resource.Composed, conn managed.ConnectionDetails, cfg ...ConnectionDetailExtractConfig) (managed.ConnectionDetails, error) {
						return nil, errBoom
					})),
				},
			},
			args: args{
				xr: &fake.Composite{},
				req: CompositionRequest{
					Revision: &v1.CompositionRevision{},
				},
			},
			want: want{
				err: errors.Wrap(errBoom, errExtractDetails),
			},
		},
		"CheckReadinessError": {
			reason: "We should return any error encountered while checking whether a composed resource is ready.",
			params: params{
				kube: &test.MockClient{
					MockUpdate: test.NewMockUpdateFn(nil),

					// Apply calls Get and Patch
					MockGet:   test.NewMockGetFn(nil),
					MockPatch: test.NewMockPatchFn(nil),
				},
				o: []PTComposerOption{
					WithTemplateAssociator(CompositionTemplateAssociatorFn(func(ctx context.Context, c resource.Composite, ct []v1.ComposedTemplate) ([]TemplateAssociation, error) {
						tas := []TemplateAssociation{{
							Template: v1.ComposedTemplate{
								Name: pointer.String("cool-resource"),
							},
						}}
						return tas, nil
					})),
					WithComposedRenderer(RendererFn(func(ctx context.Context, cp resource.Composite, cd resource.Composed, t v1.ComposedTemplate, env *Environment) error {
						return nil
					})),
					WithCompositeRenderer(RendererFn(func(ctx context.Context, cp resource.Composite, cd resource.Composed, t v1.ComposedTemplate, env *Environment) error {
						return nil
					})),
					WithComposedConnectionDetailsFetcher(ConnectionDetailsFetcherFn(func(ctx context.Context, o resource.ConnectionSecretOwner) (managed.ConnectionDetails, error) {
						return nil, nil
					})),
					WithComposedConnectionDetailsFetcher(ConnectionDetailsFetcherFn(func(ctx context.Context, cd resource.ConnectionSecretOwner) (managed.ConnectionDetails, error) {
						return nil, nil
					})),
					WithComposedReadinessChecker(ReadinessCheckerFn(func(ctx context.Context, o ConditionedObject, rc ...ReadinessCheck) (ready bool, err error) {
						return false, errBoom
					})),
				},
			},
			args: args{
				xr: &fake.Composite{},
				req: CompositionRequest{
					Revision: &v1.CompositionRevision{},
				},
			},
			want: want{
				err: errors.Wrap(errBoom, errReadiness),
			},
		},
		"CompositeApplyError": {
			reason: "We should return any error encountered while applying the Composite.",
			params: params{
				kube: &test.MockClient{
					MockUpdate: test.NewMockUpdateFn(nil),

					// Apply calls Get and Patch. We won't hit this for any
					// composed resources because none we returned by the
					// TemplateAssociator below.
					MockGet:   test.NewMockGetFn(errBoom),
					MockPatch: test.NewMockPatchFn(nil),
				},
				o: []PTComposerOption{
					WithTemplateAssociator(CompositionTemplateAssociatorFn(func(ctx context.Context, c resource.Composite, ct []v1.ComposedTemplate) ([]TemplateAssociation, error) {
						return nil, nil
					})),
					WithCompositeRenderer(RendererFn(func(ctx context.Context, cp resource.Composite, cd resource.Composed, t v1.ComposedTemplate, env *Environment) error {
						return nil
					})),
				},
			},
			args: args{
				xr: &fake.Composite{},
				req: CompositionRequest{
					Revision: &v1.CompositionRevision{},
				},
			},
			want: want{
				err: errors.Wrap(errors.Wrap(errBoom, "cannot get object"), errUpdate),
			},
		},
		"Success": {
			reason: "We should return the resources we composed, and our derived connection details.",
			params: params{
				kube: &test.MockClient{
					MockUpdate: test.NewMockUpdateFn(nil),

					// Apply uses Get and Patch.
					MockGet:   test.NewMockGetFn(nil),
					MockPatch: test.NewMockPatchFn(nil),
				},
				o: []PTComposerOption{
					WithTemplateAssociator(CompositionTemplateAssociatorFn(func(ctx context.Context, c resource.Composite, ct []v1.ComposedTemplate) ([]TemplateAssociation, error) {
						tas := []TemplateAssociation{{
							Template: v1.ComposedTemplate{
								Name: pointer.String("cool-resource"),
							},
						}}
						return tas, nil
					})),
					WithComposedRenderer(RendererFn(func(ctx context.Context, cp resource.Composite, cd resource.Composed, t v1.ComposedTemplate, env *Environment) error {
						return nil
					})),
					WithCompositeRenderer(RendererFn(func(ctx context.Context, cp resource.Composite, cd resource.Composed, t v1.ComposedTemplate, env *Environment) error {
						return nil
					})),
					WithComposedConnectionDetailsFetcher(ConnectionDetailsFetcherFn(func(ctx context.Context, o resource.ConnectionSecretOwner) (managed.ConnectionDetails, error) {
						return nil, nil
					})),
					WithComposedConnectionDetailsExtractor(ConnectionDetailsExtractorFn(func(cd resource.Composed, conn managed.ConnectionDetails, cfg ...ConnectionDetailExtractConfig) (managed.ConnectionDetails, error) {
						return details, nil
					})),
					WithComposedReadinessChecker(ReadinessCheckerFn(func(ctx context.Context, o ConditionedObject, rc ...ReadinessCheck) (ready bool, err error) {
						return true, nil
					})),
				},
			},
			args: args{
				xr: &fake.Composite{},
				req: CompositionRequest{
					Revision: &v1.CompositionRevision{},
				},
			},
			want: want{
				res: CompositionResult{
					Composed: []ComposedResource{{
						ResourceName: "cool-resource",
						Ready:        true,
					}},
					ConnectionDetails: details,
				},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {

			c := NewPTComposer(tc.params.kube, tc.params.o...)
			res, err := c.Compose(tc.args.ctx, tc.args.xr, tc.args.req)

			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("\n%s\nCompose(...): -want, +got:\n%s", tc.reason, diff)
			}

			if diff := cmp.Diff(tc.want.res, res, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("\n%s\nCompose(...): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}

func TestRender(t *testing.T) {
	ctrl := true
	tmpl, _ := json.Marshal(&fake.Managed{})
	errBoom := errors.New("boom")

	type args struct {
		ctx context.Context
		cp  resource.Composite
		cd  resource.Composed
		t   v1.ComposedTemplate
	}
	type want struct {
		cd  resource.Composed
		err error
	}
	cases := map[string]struct {
		reason string
		client client.Client
		args
		want
	}{
		"InvalidTemplate": {
			reason: "Invalid template should not be accepted",
			args: args{
				cd: &fake.Composed{},
				t:  v1.ComposedTemplate{Base: runtime.RawExtension{Raw: []byte("olala")}},
			},
			want: want{
				cd:  &fake.Composed{},
				err: errors.Wrap(errors.New("invalid character 'o' looking for beginning of value"), errUnmarshal),
			},
		},
		"NoLabel": {
			reason: "The name prefix label has to be set",
			args: args{
				cp: &fake.Composite{},
				cd: &fake.Composed{ObjectMeta: metav1.ObjectMeta{Name: "cd"}},
				t:  v1.ComposedTemplate{Base: runtime.RawExtension{Raw: tmpl}},
			},
			want: want{
				cd:  &fake.Composed{ObjectMeta: metav1.ObjectMeta{Name: "cd"}},
				err: errors.New(errNamePrefix),
			},
		},
		"DryRunError": {
			reason: "Errors dry-run creating the rendered resource to name it should be returned",
			client: &test.MockClient{MockCreate: test.NewMockCreateFn(errBoom)},
			args: args{
				cp: &fake.Composite{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					xcrd.LabelKeyNamePrefixForComposed: "ola",
					xcrd.LabelKeyClaimName:             "rola",
					xcrd.LabelKeyClaimNamespace:        "rolans",
				}}},
				cd: &fake.Composed{ObjectMeta: metav1.ObjectMeta{}},
				t:  v1.ComposedTemplate{Base: runtime.RawExtension{Raw: tmpl}},
			},
			want: want{
				cd: &fake.Composed{ObjectMeta: metav1.ObjectMeta{
					GenerateName: "ola-",
					Labels: map[string]string{
						xcrd.LabelKeyNamePrefixForComposed: "ola",
						xcrd.LabelKeyClaimName:             "rola",
						xcrd.LabelKeyClaimNamespace:        "rolans",
					},
					OwnerReferences: []metav1.OwnerReference{{Controller: &ctrl, BlockOwnerDeletion: &ctrl}},
				}},
				err: errors.Wrap(errBoom, errName),
			},
		},
		"ControllerError": {
			reason: "External controller owner references should cause an exception",
			client: &test.MockClient{MockCreate: test.NewMockCreateFn(nil)},
			args: args{
				cp: &fake.Composite{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					xcrd.LabelKeyNamePrefixForComposed: "ola",
					xcrd.LabelKeyClaimName:             "rola",
					xcrd.LabelKeyClaimNamespace:        "rolans",
				}}},
				cd: &fake.Composed{ObjectMeta: metav1.ObjectMeta{Name: "cd",
					OwnerReferences: []metav1.OwnerReference{{Controller: &ctrl, BlockOwnerDeletion: &ctrl,
						UID: "random_uid"}}}},
				t: v1.ComposedTemplate{Base: runtime.RawExtension{Raw: tmpl}},
			},
			want: want{
				cd: &fake.Composed{ObjectMeta: metav1.ObjectMeta{
					Name:         "cd",
					GenerateName: "ola-",
					Labels: map[string]string{
						xcrd.LabelKeyNamePrefixForComposed: "ola",
						xcrd.LabelKeyClaimName:             "rola",
						xcrd.LabelKeyClaimNamespace:        "rolans",
					},
					OwnerReferences: []metav1.OwnerReference{{Controller: &ctrl, BlockOwnerDeletion: &ctrl,
						UID: "random_uid"}},
				}},
				err: errors.Wrap(errors.Errorf("cd is already controlled by   (UID random_uid)"), errSetControllerRef),
			},
		},
		"Success": {
			reason: "Configuration should result in the right object with correct generateName",
			client: &test.MockClient{MockCreate: test.NewMockCreateFn(nil)},
			args: args{
				cp: &fake.Composite{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					xcrd.LabelKeyNamePrefixForComposed: "ola",
					xcrd.LabelKeyClaimName:             "rola",
					xcrd.LabelKeyClaimNamespace:        "rolans",
				}}},
				cd: &fake.Composed{ObjectMeta: metav1.ObjectMeta{Name: "cd"}},
				t:  v1.ComposedTemplate{Base: runtime.RawExtension{Raw: tmpl}},
			},
			want: want{
				cd: &fake.Composed{ObjectMeta: metav1.ObjectMeta{
					Name:         "cd",
					GenerateName: "ola-",
					Labels: map[string]string{
						xcrd.LabelKeyNamePrefixForComposed: "ola",
						xcrd.LabelKeyClaimName:             "rola",
						xcrd.LabelKeyClaimNamespace:        "rolans",
					},
					OwnerReferences: []metav1.OwnerReference{{Controller: &ctrl, BlockOwnerDeletion: &ctrl}},
				}},
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := NewAPIDryRunRenderer(tc.client)
			err := r.Render(tc.args.ctx, tc.args.cp, tc.args.cd, tc.args.t, nil)
			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("\n%s\nRender(...): -want, +got:\n%s", tc.reason, diff)
			}
			if diff := cmp.Diff(tc.want.cd, tc.args.cd); diff != "" {
				t.Errorf("\n%s\nRender(...): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}

func TestAssociateByOrder(t *testing.T) {
	t0 := v1.ComposedTemplate{Base: runtime.RawExtension{Raw: []byte("zero")}}
	t1 := v1.ComposedTemplate{Base: runtime.RawExtension{Raw: []byte("one")}}
	t2 := v1.ComposedTemplate{Base: runtime.RawExtension{Raw: []byte("two")}}

	r0 := corev1.ObjectReference{Name: "zero"}
	r1 := corev1.ObjectReference{Name: "one"}
	r2 := corev1.ObjectReference{Name: "two"}

	cases := map[string]struct {
		reason string
		t      []v1.ComposedTemplate
		r      []corev1.ObjectReference
		want   []TemplateAssociation
	}{
		"NoReferences": {
			reason: "When there are no references we should return templates associated with empty references.",
			t:      []v1.ComposedTemplate{t0, t1, t2},
			want: []TemplateAssociation{
				{Template: t0},
				{Template: t1},
				{Template: t2},
			},
		},
		"SomeReferences": {
			reason: "We should return all templates when there are fewer references than templates.",
			t:      []v1.ComposedTemplate{t0, t1, t2},
			r:      []corev1.ObjectReference{r0, r1},
			want: []TemplateAssociation{
				{Template: t0, Reference: r0},
				{Template: t1, Reference: r1},
				{Template: t2},
			},
		},
		"ExtraReferences": {
			reason: "When there are more references than templates they should be truncated.",
			t:      []v1.ComposedTemplate{t0, t1},
			r:      []corev1.ObjectReference{r0, r1, r2},
			want: []TemplateAssociation{
				{Template: t0, Reference: r0},
				{Template: t1, Reference: r1},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := AssociateByOrder(tc.t, tc.r)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("\n%s\nAssociateByOrder(...): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}

func TestGarbageCollectingAssociator(t *testing.T) {
	errBoom := errors.New("boom")

	n0 := "zero"
	t0 := v1.ComposedTemplate{Name: &n0}

	r0 := corev1.ObjectReference{Name: n0}

	type args struct {
		ctx context.Context
		cr  resource.Composite
		ct  []v1.ComposedTemplate
	}

	type want struct {
		tas []TemplateAssociation
		err error
	}

	cases := map[string]struct {
		reason string
		c      client.Client
		args   args
		want   want
	}{
		"AnonymousTemplates": {
			reason: "We should fall back to associating templates with references by order if any template is not named.",
			args: args{
				cr: &fake.Composite{},
				ct: []v1.ComposedTemplate{t0, {Name: nil}},
			},
			want: want{
				tas: []TemplateAssociation{{Template: t0}, {Template: v1.ComposedTemplate{Name: nil}}},
			},
		},
		"ResourceNotFoundError": {
			reason: "Non-existent resources should be ignored.",
			c: &test.MockClient{
				MockGet: test.NewMockGetFn(kerrors.NewNotFound(schema.GroupResource{}, "")),
			},
			args: args{
				cr: &fake.Composite{
					ComposedResourcesReferencer: fake.ComposedResourcesReferencer{Refs: []corev1.ObjectReference{r0}},
				},
				ct: []v1.ComposedTemplate{t0},
			},
			want: want{
				tas: []TemplateAssociation{{Template: t0}},
			},
		},
		"GetResourceError": {
			reason: "Errors getting a referenced resource should be returned.",
			c: &test.MockClient{
				MockGet: test.NewMockGetFn(errBoom),
			},
			args: args{
				cr: &fake.Composite{
					ComposedResourcesReferencer: fake.ComposedResourcesReferencer{Refs: []corev1.ObjectReference{r0}},
				},
				ct: []v1.ComposedTemplate{t0},
			},
			want: want{
				err: errors.Wrap(errBoom, errGetComposed),
			},
		},
		"AnonymousResource": {
			reason: "We should fall back to associating templates with references by order if any resource is not annotated with its template name.",
			c: &test.MockClient{
				// Return an empty (and thus unannotated) composed resource.
				MockGet: test.NewMockGetFn(nil),
			},
			args: args{
				cr: &fake.Composite{
					ComposedResourcesReferencer: fake.ComposedResourcesReferencer{Refs: []corev1.ObjectReference{r0}},
				},
				ct: []v1.ComposedTemplate{t0},
			},
			want: want{
				tas: []TemplateAssociation{{Template: t0, Reference: r0}},
			},
		},
		"AssociatedResource": {
			reason: "We should associate referenced resources by their template name annotation.",
			c: &test.MockClient{
				MockGet: test.NewMockGetFn(nil, func(obj client.Object) error {
					SetCompositionResourceName(obj.(metav1.Object), n0)
					return nil
				}),
			},
			args: args{
				cr: &fake.Composite{
					ComposedResourcesReferencer: fake.ComposedResourcesReferencer{Refs: []corev1.ObjectReference{r0}},
				},
				ct: []v1.ComposedTemplate{t0},
			},
			want: want{
				tas: []TemplateAssociation{{Template: t0, Reference: r0}},
			},
		},
		"UncontrolledResource": {
			reason: "We should not garbage collect a resource that we don't control.",
			c: &test.MockClient{
				MockGet: test.NewMockGetFn(nil, func(obj client.Object) error {
					// The template used to create this resource is no longer known to us.
					SetCompositionResourceName(obj, "unknown")

					// This resource is not controlled by us.
					ctrl := true
					obj.SetOwnerReferences([]metav1.OwnerReference{{
						Controller:         &ctrl,
						BlockOwnerDeletion: &ctrl,
						UID:                types.UID("who-dat"),
					}})
					return nil
				}),
			},
			args: args{
				cr: &fake.Composite{
					ObjectMeta:                  metav1.ObjectMeta{UID: types.UID("very-unique")},
					ComposedResourcesReferencer: fake.ComposedResourcesReferencer{Refs: []corev1.ObjectReference{r0}},
				},
				ct: []v1.ComposedTemplate{t0},
			},
			want: want{
				tas: []TemplateAssociation{{Template: t0}},
			},
		},
		"GarbageCollectionError": {
			reason: "We should return errors encountered while garbage collecting a composed resource.",
			c: &test.MockClient{
				MockGet: test.NewMockGetFn(nil, func(obj client.Object) error {
					// The template used to create this resource is no longer known to us.
					SetCompositionResourceName(obj, "unknown")
					return nil
				}),
				MockDelete: test.NewMockDeleteFn(errBoom),
			},
			args: args{
				cr: &fake.Composite{
					ComposedResourcesReferencer: fake.ComposedResourcesReferencer{Refs: []corev1.ObjectReference{r0}},
				},
				ct: []v1.ComposedTemplate{t0},
			},
			want: want{
				err: errors.Wrap(errBoom, errGCComposed),
			},
		},
		"GarbageCollectedResource": {
			reason: "We should not return a resource that we successfully garbage collect.",
			c: &test.MockClient{
				MockGet: test.NewMockGetFn(nil, func(obj client.Object) error {
					// The template used to create this resource is no longer known to us.
					SetCompositionResourceName(obj, "unknown")
					return nil
				}),
				MockDelete: test.NewMockDeleteFn(nil),
			},
			args: args{
				cr: &fake.Composite{
					ComposedResourcesReferencer: fake.ComposedResourcesReferencer{Refs: []corev1.ObjectReference{r0}},
				},
				ct: []v1.ComposedTemplate{t0},
			},
			want: want{
				tas: []TemplateAssociation{{Template: t0}},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a := NewGarbageCollectingAssociator(tc.c)
			got, err := a.AssociateTemplates(tc.args.ctx, tc.args.cr, tc.args.ct)

			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("\n%s\nAssociateTemplates(...): -want, +got:\n%s", tc.reason, diff)
			}
			if diff := cmp.Diff(tc.want.tas, got); diff != "" {
				t.Errorf("\n%s\nAssociateTemplates(...): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}
