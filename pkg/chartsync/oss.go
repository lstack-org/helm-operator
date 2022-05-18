package chartsync

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"fmt"
	"github.com/aliyun/aliyun-oss-go-sdk/oss"
	v1 "github.com/fluxcd/helm-operator/pkg/apis/helm.fluxcd.io/v1"
	"github.com/huaweicloud/huaweicloud-sdk-go-obs/obs"
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
	DownloadFile() (string, error)
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

func (a *aliImpl) DownloadFile() (string, error) {
	AckDecode(a.Oss)
	client, err := oss.New(a.Endpoint(a.RegionId), a.AckId, a.AckSecret)
	if err != nil {
		return "", ChartUnavailableError{err}
	}

	bucket, err := client.Bucket(a.Bucket)
	if err != nil {
		return "", ChartUnavailableError{err}
	}

	cachePath := filepath.Join(a.base, base64.URLEncoding.EncodeToString([]byte(a.Key)))
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

func (h *huaweiImpl) DownloadFile() (string, error) {
	AckDecode(h.Oss)
	client, err := obs.New(h.AckId, h.AckSecret, h.Endpoint(h.RegionId))
	if err != nil {
		return "", ChartUnavailableError{err}
	}

	cachePath := filepath.Join(h.base, base64.URLEncoding.EncodeToString([]byte(h.Key)))

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

func AckDecode(oss *v1.Oss) {
	if oss.AckEncrypted {
		oss.AckId = AesDecrypt(oss.AckId)
		oss.AckSecret = AesDecrypt(oss.AckSecret)
	}
}

// AesDecrypt aes解密
func AesDecrypt(ciphertext string) string {
	//使用RawURLEncoding 不要使用StdEncoding
	//不要使用StdEncoding  放在url参数中回导致错误
	decryptedByte, _ := base64.RawURLEncoding.DecodeString(ciphertext)
	k := []byte("2367943245267894")

	// 分组密钥
	block, err := aes.NewCipher(k)
	if err != nil {
		panic(fmt.Sprintf("key 长度必须 16/24/32长度: %s", err.Error()))
	}
	// 获取密钥块的长度
	blockSize := block.BlockSize()
	// 加密模式
	blockMode := cipher.NewCBCDecrypter(block, k[:blockSize])
	// 创建数组
	orig := make([]byte, len(decryptedByte))
	// 解密
	blockMode.CryptBlocks(orig, decryptedByte)
	// 去补全码
	orig = PKCS7UnPadding(orig)
	return string(orig)
}


// PKCS7UnPadding 去码
func PKCS7UnPadding(origData []byte) []byte {
	length := len(origData)
	unPadding := int(origData[length-1])
	return origData[:(length - unPadding)]
}
