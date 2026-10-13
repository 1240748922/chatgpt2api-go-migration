package remotestore

import (
 "bytes"
 "context"
 "crypto/aes"
 "crypto/cipher"
 "crypto/sha256"
 "encoding/hex"
 "fmt"
 "io"
 "net/http"
 "net/http/httptest"
 "net/url"
 "strings"
 "testing"

 "golang.org/x/crypto/pbkdf2"
)

func TestBackupEncryptionOpenSSLFormat(t *testing.T){
 for _,size:=range []int{0,1,15,16,17,65535,65536,65537,131072}{t.Run(fmt.Sprint(size),func(t *testing.T){
  plain:=bytes.Repeat([]byte{byte(size)},size);var out bytes.Buffer
  if err:=EncryptBackup(&out,bytes.NewReader(plain),"test passphrase");err!=nil{t.Fatal(err)}
  raw:=out.Bytes();if len(raw)<32 || string(raw[:8])!="Salted__" || (len(raw)-16)%16!=0{t.Fatal("invalid OpenSSL framing")}
  key:=pbkdf2.Key([]byte("test passphrase"),raw[8:16],10000,48,sha256.New);block,_:=aes.NewCipher(key[:32]);decoded:=bytes.Clone(raw[16:]);cipher.NewCBCDecrypter(block,key[32:]).CryptBlocks(decoded,decoded)
  padding:=int(decoded[len(decoded)-1]);if padding<1 || padding>16{t.Fatal("invalid padding")};for _,b:=range decoded[len(decoded)-padding:]{if int(b)!=padding{t.Fatal("padding mismatch")}}
  if !bytes.Equal(decoded[:len(decoded)-padding],plain){t.Fatal("decrypted archive differs")}
 })}
}

func TestWebDAVLifecycleAndRedirectProtection(t *testing.T){
 objects:=map[string][]byte{};collections:=[]string{}
 server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
  u,p,ok:=r.BasicAuth();if !ok || u!="test" || p!="secret"{w.WriteHeader(401);return}
  switch r.Method{case "MKCOL":collections=append(collections,r.URL.Path);w.WriteHeader(201)
  case "PUT":objects[r.URL.Path],_=io.ReadAll(r.Body);w.WriteHeader(201)
  case "GET":data,ok:=objects[r.URL.Path];if !ok{w.WriteHeader(404)}else{w.Write(data)}
  case "DELETE":delete(objects,r.URL.Path);w.WriteHeader(204)}
 }));defer server.Close()
 dav:=&WebDAV{URL:server.URL+"/dav",Root:"a/b",Username:"test",Password:"secret"};ctx:=context.Background()
 if err:=dav.Put(ctx,"image ü.png",[]byte("image"));err!=nil{t.Fatal(err)}
 if len(collections)!=2 || collections[0]!="/dav/a" || collections[1]!="/dav/a/b"{t.Fatal(collections)}
 got,err:=dav.Get(ctx,"image ü.png");if err!=nil || string(got)!="image"{t.Fatalf("get: %s %v",got,err)}
 if err=dav.Delete(ctx,"image ü.png");err!=nil{t.Fatal(err)};if err=dav.Test(ctx,"probe");err!=nil || len(objects)!=0{t.Fatalf("probe left objects: %v",err)}
 for _,rel:=range []string{"../image.png","x/../image.png","/image.png","x\\image.png"}{if err=dav.Put(ctx,rel,nil);err==nil{t.Fatal("unsafe path accepted",rel)}}
 redirect:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){http.Redirect(w,r,server.URL+"/dav/stolen",307)}));defer redirect.Close();dav.URL=redirect.URL
 if err=dav.Put(ctx,"image.png",nil);err==nil{t.Fatal("redirect was accepted")}
}

func TestR2SignedRoundTripAndPagination(t *testing.T){
 objects:=map[string][]byte{};pages:=0
 server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
  data,_:=io.ReadAll(r.Body);sum:=sha256.Sum256(data);digest:=hex.EncodeToString(sum[:]);if r.Header.Get("X-Amz-Content-Sha256")!=digest{t.Error("payload hash mismatch")}
  stamp:=r.Header.Get("X-Amz-Date");if len(stamp)!=16{t.Error("invalid date");return};date:=stamp[:8];scope:=date+"/auto/s3/aws4_request"
  canonical:=r.Method+"\n"+r.URL.EscapedPath()+"\n"+strings.ReplaceAll(r.URL.Query().Encode(),"+","%20")+"\n"+"host:"+r.Host+"\nx-amz-content-sha256:"+digest+"\nx-amz-date:"+stamp+"\n\nhost;x-amz-content-sha256;x-amz-date\n"+digest
  hashed:=sha256.Sum256([]byte(canonical));key:=awsHMAC([]byte("AWS4test-secret"),date);for _,part:=range []string{"auto","s3","aws4_request"}{key=awsHMAC(key,part)}
  expected:=hex.EncodeToString(awsHMAC(key,"AWS4-HMAC-SHA256\n"+stamp+"\n"+scope+"\n"+hex.EncodeToString(hashed[:])))
  if !strings.HasSuffix(r.Header.Get("Authorization"),"Signature="+expected){t.Error("signature mismatch")}
  if r.URL.Query().Get("list-type")=="2"{pages++;if pages==1{w.Write([]byte(`<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>a+b/ü</NextContinuationToken><Contents><Key>backup/a.zip</Key><Size>3</Size></Contents></ListBucketResult>`))}else{if r.URL.Query().Get("continuation-token")!="a+b/ü"{t.Error("cursor changed")};w.Write([]byte(`<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>backup/b.zip</Key><Size>4</Size></Contents></ListBucketResult>`))};return}
  switch r.Method{case "PUT":objects[r.URL.Path]=data;case "GET":w.Write(objects[r.URL.Path]);case "DELETE":delete(objects,r.URL.Path)}
 }));defer server.Close();r2:=&R2{Endpoint:server.URL,AccessKey:"test-key",Secret:"test-secret",Bucket:"bucket",Prefix:"backup"};ctx:=context.Background();key:=r2.Key("x ü.zip")
 if err:=r2.Put(ctx,key,bytes.NewReader([]byte("archive")));err!=nil{t.Fatal(err)};reader,err:=r2.Get(ctx,key);if err!=nil{t.Fatal(err)};data,_:=io.ReadAll(reader);reader.Close();if string(data)!="archive"{t.Fatal(string(data))}
 items,err:=r2.List(ctx);if err!=nil || len(items)!=2{t.Fatalf("list: %v %v",items,err)};if err=r2.Delete(ctx,key);err!=nil || len(objects)!=0{t.Fatal(err)}
 if got:=(&R2{}).Key("name.zip");got!="name.zip"{t.Fatal(got)}
 if _,err=r2.signed(ctx,"PUT","../secret",url.Values{},nil);err==nil{t.Fatal("traversal accepted")}
}
