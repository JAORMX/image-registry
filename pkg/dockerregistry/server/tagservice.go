package server

import (
	"github.com/docker/distribution"
	"github.com/docker/distribution/context"
	"github.com/docker/distribution/digest"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	imageapiv1 "github.com/openshift/api/image/v1"
	imageapi "github.com/openshift/image-registry/pkg/origin-common/image/apis/image"
	quotautil "github.com/openshift/image-registry/pkg/origin-common/quota/util"
	util "github.com/openshift/image-registry/pkg/origin-common/util"
)

type tagService struct {
	distribution.TagService

	imageStream        *imageStream
	pullthroughEnabled bool
}

func (t tagService) Get(ctx context.Context, tag string) (distribution.Descriptor, error) {
	imageStream, err := t.imageStream.imageStreamGetter.get()
	if err != nil {
		context.GetLogger(ctx).Errorf("error retrieving ImageStream %s: %v", t.imageStream.Reference(), err)
		return distribution.Descriptor{}, distribution.ErrRepositoryUnknown{Name: t.imageStream.Reference()}
	}

	te := util.LatestTaggedImage(imageStream, tag)
	if te == nil {
		return distribution.Descriptor{}, distribution.ErrTagUnknown{Tag: tag}
	}
	dgst, err := digest.ParseDigest(te.Image)
	if err != nil {
		return distribution.Descriptor{}, err
	}

	if !t.pullthroughEnabled {
		image, err := t.imageStream.getImage(ctx, dgst)
		if err != nil {
			return distribution.Descriptor{}, err
		}

		if !isImageManaged(image) {
			return distribution.Descriptor{}, distribution.ErrTagUnknown{Tag: tag}
		}
	}

	return distribution.Descriptor{Digest: dgst}, nil
}

func (t tagService) All(ctx context.Context) ([]string, error) {
	tags := []string{}

	imageStream, err := t.imageStream.imageStreamGetter.get()
	if err != nil {
		context.GetLogger(ctx).Errorf("error retrieving ImageStream %s: %v", t.imageStream.Reference(), err)
		return tags, distribution.ErrRepositoryUnknown{Name: t.imageStream.Reference()}
	}

	managedImages := make(map[string]bool)

	for _, history := range imageStream.Status.Tags {
		if len(history.Items) == 0 {
			continue
		}
		tag := history.Tag

		if t.pullthroughEnabled {
			tags = append(tags, tag)
			continue
		}

		managed, found := managedImages[history.Items[0].Image]
		if !found {
			dgst, err := digest.ParseDigest(history.Items[0].Image)
			if err != nil {
				context.GetLogger(ctx).Errorf("bad digest %s: %v", history.Items[0].Image, err)
				continue
			}

			image, err := t.imageStream.getImage(ctx, dgst)
			if err != nil {
				context.GetLogger(ctx).Errorf("unable to get image %s %s: %v", t.imageStream.Reference(), dgst.String(), err)
				continue
			}
			managed = isImageManaged(image)
			managedImages[history.Items[0].Image] = managed
		}

		if !managed {
			continue
		}

		tags = append(tags, tag)
	}
	return tags, nil
}

func (t tagService) Lookup(ctx context.Context, desc distribution.Descriptor) ([]string, error) {
	tags := []string{}

	imageStream, err := t.imageStream.imageStreamGetter.get()
	if err != nil {
		context.GetLogger(ctx).Errorf("error retrieving ImageStream %s: %v", t.imageStream.Reference(), err)
		return tags, distribution.ErrRepositoryUnknown{Name: t.imageStream.Reference()}
	}

	managedImages := make(map[string]bool)

	for _, history := range imageStream.Status.Tags {
		if len(history.Items) == 0 {
			continue
		}
		tag := history.Tag

		dgst, err := digest.ParseDigest(history.Items[0].Image)
		if err != nil {
			context.GetLogger(ctx).Errorf("bad digest %s: %v", history.Items[0].Image, err)
			continue
		}

		if dgst != desc.Digest {
			continue
		}

		if t.pullthroughEnabled {
			tags = append(tags, tag)
			continue
		}

		managed, found := managedImages[history.Items[0].Image]
		if !found {
			image, err := t.imageStream.getImage(ctx, dgst)
			if err != nil {
				context.GetLogger(ctx).Errorf("unable to get image %s %s: %v", t.imageStream.Reference(), dgst.String(), err)
				continue
			}
			managed = isImageManaged(image)
			managedImages[history.Items[0].Image] = managed
		}

		if !managed {
			continue
		}

		tags = append(tags, tag)
	}

	return tags, nil
}

func (t tagService) Tag(ctx context.Context, tag string, dgst distribution.Descriptor) error {
	imageStream, err := t.imageStream.imageStreamGetter.get()
	if err != nil {
		context.GetLogger(ctx).Errorf("error retrieving ImageStream %s: %v", t.imageStream.Reference(), err)
		return distribution.ErrRepositoryUnknown{Name: t.imageStream.Reference()}
	}

	image, err := t.imageStream.registryOSClient.Images().Get(dgst.Digest.String(), metav1.GetOptions{})
	if err != nil {
		context.GetLogger(ctx).Errorf("unable to get image: %s", dgst.Digest.String())
		return err
	}
	image.SetResourceVersion("")

	if !t.pullthroughEnabled && !isImageManaged(image) {
		return distribution.ErrRepositoryUnknown{Name: t.imageStream.Reference()}
	}

	ism := imageapiv1.ImageStreamMapping{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: imageStream.Namespace,
			Name:      imageStream.Name,
		},
		Tag:   tag,
		Image: *image,
	}

	_, err = t.imageStream.registryOSClient.ImageStreamMappings(imageStream.Namespace).Create(&ism)
	if quotautil.IsErrorQuotaExceeded(err) {
		context.GetLogger(ctx).Errorf("denied creating ImageStreamMapping: %v", err)
		return distribution.ErrAccessDenied
	}

	return err
}

func (t tagService) Untag(ctx context.Context, tag string) error {
	imageStream, err := t.imageStream.imageStreamGetter.get()
	if err != nil {
		context.GetLogger(ctx).Errorf("error retrieving ImageStream %s: %v", t.imageStream.Reference(), err)
		return distribution.ErrRepositoryUnknown{Name: t.imageStream.Reference()}
	}

	te := util.LatestTaggedImage(imageStream, tag)
	if te == nil {
		return distribution.ErrTagUnknown{Tag: tag}
	}

	if !t.pullthroughEnabled {
		dgst, err := digest.ParseDigest(te.Image)
		if err != nil {
			return err
		}

		image, err := t.imageStream.getImage(ctx, dgst)
		if err != nil {
			return err
		}

		if !isImageManaged(image) {
			return distribution.ErrTagUnknown{Tag: tag}
		}
	}

	return t.imageStream.registryOSClient.ImageStreamTags(imageStream.Namespace).Delete(imageapi.JoinImageStreamTag(imageStream.Name, tag), &metav1.DeleteOptions{})
}
