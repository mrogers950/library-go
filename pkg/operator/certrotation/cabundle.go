package certrotation

import (
	"crypto/x509"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1informers "k8s.io/client-go/informers/core/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/cert"

	"github.com/openshift/library-go/pkg/crypto"
	"github.com/openshift/library-go/pkg/operator/events"
)

// CABundleRotation maintains a CA bundle config map, but adding new CA certs and removing expired old ones.
type CABundleRotation struct {
	Namespace string
	Name      string

	Informer      corev1informers.ConfigMapInformer
	Lister        corev1listers.ConfigMapLister
	Client        corev1client.ConfigMapsGetter
	EventRecorder events.Recorder
}

func (c CABundleRotation) ensureConfigMapCABundle(signingCertKeyPair *crypto.CA) error {
	// by this point we have current signing cert/key pair.  We now need to make sure that the ca-bundle configmap has this cert and
	// doesn't have any expired certs
	originalCABundleConfigMap, err := c.Lister.ConfigMaps(c.Namespace).Get(c.Name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	caBundleConfigMap := originalCABundleConfigMap.DeepCopy()
	if apierrors.IsNotFound(err) {
		// create an empty one
		caBundleConfigMap = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: c.Namespace, Name: c.Name}}
	}
	if err := manageCABundleConfigMap(caBundleConfigMap, signingCertKeyPair.Config.Certs[0]); err != nil {
		return err
	}
	if originalCABundleConfigMap == nil || originalCABundleConfigMap.Data == nil || !equality.Semantic.DeepEqual(originalCABundleConfigMap.Data, caBundleConfigMap.Data) {
		c.EventRecorder.Eventf("CABundleUpdateRequired", "%q in %q requires a new cert", c.Namespace, c.Name)
		actualCABundleConfigMap, err := c.Client.ConfigMaps(c.Namespace).Update(caBundleConfigMap)
		if apierrors.IsNotFound(err) {
			actualCABundleConfigMap, err = c.Client.ConfigMaps(c.Namespace).Create(caBundleConfigMap)
			if err != nil {
				return err
			}
		}
		if err != nil {
			return err
		}
		caBundleConfigMap = actualCABundleConfigMap
	}

	return nil
}

// manageCABundleConfigMap adds the new certificate to the list of cabundles, eliminates duplicates, and prunes the list of expired
// certs to trust as signers
func manageCABundleConfigMap(caBundleConfigMap *corev1.ConfigMap, currentSigner *x509.Certificate) error {
	if caBundleConfigMap.Data == nil {
		caBundleConfigMap.Data = map[string]string{}
	}

	certificates := []*x509.Certificate{}
	caBundle := caBundleConfigMap.Data["ca-bundle.crt"]
	if len(caBundle) > 0 {
		var err error
		certificates, err = cert.ParseCertsPEM([]byte(caBundle))
		if err != nil {
			return err
		}
	}
	certificates = append([]*x509.Certificate{currentSigner}, certificates...)
	certificates = crypto.FilterExpiredCerts(certificates...)

	finalCertificates := []*x509.Certificate{}
	// now check for duplicates. n^2, but super simple
	for i := range certificates {
		found := false
		for j := range finalCertificates {
			if reflect.DeepEqual(certificates[i].Raw, finalCertificates[j].Raw) {
				found = true
				break
			}
		}
		if !found {
			finalCertificates = append(finalCertificates, certificates[i])
		}
	}

	caBytes, err := crypto.EncodeCertificates(finalCertificates...)
	if err != nil {
		return err
	}
	caBundleConfigMap.Data["ca-bundle.crt"] = string(caBytes)

	return nil
}
