package remotestore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"golang.org/x/crypto/pbkdf2"
	"io"
)

// Compatible with openssl enc -aes-256-cbc -pbkdf2 -iter 10000 -md sha256 -saltlen 8.
func EncryptBackup(destination io.Writer, source io.Reader, passphrase string) error {
	if passphrase == "" {
		return fmt.Errorf("backup passphrase required")
	}
	salt := make([]byte, 8)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	key := pbkdf2.Key([]byte(passphrase), salt, 10000, 48, sha256.New)
	block, err := aes.NewCipher(key[:32])
	if err != nil {
		return err
	}
	if _, err = destination.Write(append([]byte("Salted__"), salt...)); err != nil {
		return err
	}
	mode := cipher.NewCBCEncrypter(block, key[32:])
	buf := make([]byte, 64<<10)
	for {
		n, e := io.ReadFull(source, buf)
		if e != nil && e != io.EOF && e != io.ErrUnexpectedEOF {
			return e
		}
		if e == nil {
			mode.CryptBlocks(buf, buf)
			if _, err = destination.Write(buf); err != nil {
				return err
			}
			continue
		}
		pad := aes.BlockSize - n%aes.BlockSize
		final := make([]byte, n+pad)
		copy(final, buf[:n])
		for i := n; i < len(final); i++ {
			final[i] = byte(pad)
		}
		mode.CryptBlocks(final, final)
		_, err = destination.Write(final)
		return err
	}
}
