package chartsync

import (
	"fmt"
	"testing"
)

func TestAESDecrypt(t *testing.T) {
	source:="myuIS5j0sZldKX06Qt13EaFhoBjN4T"
	fmt.Println("原字符：",source)
	encryptCode :=AesEncrypt(source)

	t.Log(encryptCode)
	decryptCode :=AesDecrypt(encryptCode)

	fmt.Println("解密",string(decryptCode))

}
