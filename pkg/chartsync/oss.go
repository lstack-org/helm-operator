package chartsync

import (
	"crypto/aes"
	"encoding/base64"
	"fmt"
	"github.com/aliyun/aliyun-oss-go-sdk/oss"
	v1 "github.com/lstack-org/helm-operator/pkg/apis/helm.fluxcd.io/v1"
	"github.com/huaweicloud/huaweicloud-sdk-go-obs/obs"
	"k8s.io/klog"
	"os"
	"path/filepath"
)

const (
	//Ali 阿里云
	Ali = "aliyun"
	//Huawei 华为云
	Huawei = "huaweiyun"
)

func NewProvider(oss *v1.Oss, base string) (Provider, error) {
	switch oss.CloudProvider {
	case Ali:
		return &aliImpl{
			Oss:  oss,
			base: base,
		}, nil
	case Huawei:
		return &huaweiImpl{
			Oss:  oss,
			base: base,
		}, nil
	}
	return nil, ChartUnavailableError{fmt.Errorf("unknown cloudProvider :%s", oss.CloudProvider)}
}

type Provider interface {
	//DownloadFile 将对象存储中的文件下载到本地
	//返回文件缓存目录
	//useCache 表示使用缓存
	DownloadFile(useCache bool) (string, error)
	//Endpoint 将region转换为oss endpoint
	Endpoint(regionId string) string
}

var (
	_ Provider = new(aliImpl)
	_ Provider = new(huaweiImpl)
)

type aliImpl struct {
	*v1.Oss
	base string
}

func (a *aliImpl) DownloadFile(useCache bool) (string, error) {
	cachePath := filepath.Join(a.base, base64.URLEncoding.EncodeToString([]byte(a.Key)))
	if useCache {
		klog.Infof("cache used,key: %s,path:%s", a.Key, cachePath)
		_, err := os.Stat(cachePath)
		//文件存在
		if err == nil {
			return cachePath, nil
		}
	}

	err := AckDecode(a.Oss)
	if err != nil {
		return "", err
	}
	client, err := oss.New(a.Endpoint(a.RegionId), a.AckId, a.AckSecret)
	if err != nil {
		return "", ChartUnavailableError{err}
	}

	bucket, err := client.Bucket(a.Bucket)
	if err != nil {
		return "", ChartUnavailableError{err}
	}

	err = bucket.GetObjectToFile(a.Key, cachePath)
	if err != nil {
		return "", ChartUnavailableError{err}
	}
	return cachePath, nil
}

func (a *aliImpl) Endpoint(regionId string) string {
	return fmt.Sprintf("http://%s.aliyuncs.com", regionId)
}

type huaweiImpl struct {
	*v1.Oss
	base string
}

func (h *huaweiImpl) DownloadFile(useCache bool) (string, error) {
	cachePath := filepath.Join(h.base, base64.URLEncoding.EncodeToString([]byte(h.Key)))
	if useCache {
		klog.Infof("cache used,key: %s,path:%s", h.Key, cachePath)
		_, err := os.Stat(cachePath)
		//文件存在
		if err == nil {
			return cachePath, nil
		}
	}

	err := AckDecode(h.Oss)
	if err != nil {
		return "", err
	}
	client, err := obs.New(h.AckId, h.AckSecret, h.Endpoint(h.RegionId))
	if err != nil {
		return "", ChartUnavailableError{err}
	}

	defer client.Close()
	_, err = client.DownloadFile(&obs.DownloadFileInput{
		GetObjectMetadataInput: obs.GetObjectMetadataInput{
			Bucket: h.Bucket,
			Key:    h.Key,
		},
		DownloadFile: cachePath,
	})
	if err != nil {
		return "", ChartUnavailableError{err}
	}
	return cachePath, nil
}

func (h *huaweiImpl) Endpoint(regionId string) string {
	return fmt.Sprintf("http://obs.%s.myhuaweicloud.com", regionId)
}

func AckDecode(oss *v1.Oss) error {
	if oss.AckEncrypted {
		ackId, err := Decrypt(oss.AckId)
		if err != nil {
			return err
		}
		ackSecret, err := Decrypt(oss.AckSecret)
		if err != nil {
			return err
		}

		oss.AckId = ackId
		oss.AckSecret = ackSecret
	}
	return nil
}

func Decrypt(encrypted string) (string, error) {
	defer func() {
		if err := recover(); err != nil {
			klog.Error(err)
		}
	}()
	bytes, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return "", err
	}
	k := []byte("2367943245267894")
	decrypted := AESDecrypt(bytes, k)
	return string(decrypted), nil
}

func AESDecrypt(encrypted []byte, key []byte) (decrypted []byte) {
	cipher, _ := aes.NewCipher(key)
	decrypted = make([]byte, len(encrypted))
	//
	for bs, be := 0, cipher.BlockSize(); bs < len(encrypted); bs, be = bs+cipher.BlockSize(), be+cipher.BlockSize() {
		cipher.Decrypt(decrypted[bs:be], encrypted[bs:be])
	}
	trim := 0
	if len(decrypted) > 0 {
		trim = len(decrypted) - int(decrypted[len(decrypted)-1])
	}
	return decrypted[:trim]
}
