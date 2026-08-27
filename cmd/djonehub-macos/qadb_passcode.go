package main

import (
	"crypto/md5"
	"errors"
	"strings"
)

const (
	qadbCryptPassword = "SH_adb_quectel"
	qadbPasscodeSize  = 15
	md5CryptAlphabet  = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

// generateQADBPasscode reproduces the module unlock algorithm used by the
// reference passcode.py: md5_crypt("SH_adb_quectel", salt=challenge), followed
// by the first 15 characters of the checksum field.
func generateQADBPasscode(challenge string) (string, error) {
	if !qadbChallengeValuePattern.MatchString(challenge) {
		return "", errors.New("QADBKEY challenge 格式无效；需要 1–8 位数字")
	}
	checksum := md5CryptChecksum(qadbCryptPassword, challenge)
	if len(checksum) < qadbPasscodeSize {
		return "", errors.New("无法生成 QADBKEY passcode")
	}
	passcode := checksum[:qadbPasscodeSize]
	if err := validateQADBPasscode(passcode); err != nil {
		return "", err
	}
	return passcode, nil
}

// md5CryptChecksum implements the checksum portion of the $1$ md5-crypt
// format. QADBKEY challenges are already validated as salts of at most 8 bytes.
func md5CryptChecksum(password, salt string) string {
	passwordBytes := []byte(password)
	saltBytes := []byte(salt)

	primary := md5.New()
	primary.Write(passwordBytes)
	primary.Write([]byte("$1$"))
	primary.Write(saltBytes)

	alternate := md5.New()
	alternate.Write(passwordBytes)
	alternate.Write(saltBytes)
	alternate.Write(passwordBytes)
	alternateDigest := alternate.Sum(nil)
	for remaining := len(passwordBytes); remaining > 0; remaining -= md5.Size {
		count := remaining
		if count > md5.Size {
			count = md5.Size
		}
		primary.Write(alternateDigest[:count])
	}
	for length := len(passwordBytes); length > 0; length >>= 1 {
		if length&1 != 0 {
			primary.Write([]byte{0})
		} else {
			primary.Write(passwordBytes[:1])
		}
	}
	digest := primary.Sum(nil)

	for round := 0; round < 1000; round++ {
		next := md5.New()
		if round&1 != 0 {
			next.Write(passwordBytes)
		} else {
			next.Write(digest)
		}
		if round%3 != 0 {
			next.Write(saltBytes)
		}
		if round%7 != 0 {
			next.Write(passwordBytes)
		}
		if round&1 != 0 {
			next.Write(digest)
		} else {
			next.Write(passwordBytes)
		}
		digest = next.Sum(nil)
	}

	var encoded strings.Builder
	encoded.Grow(22)
	writeMD5CryptBase64(&encoded, digest[0], digest[6], digest[12], 4)
	writeMD5CryptBase64(&encoded, digest[1], digest[7], digest[13], 4)
	writeMD5CryptBase64(&encoded, digest[2], digest[8], digest[14], 4)
	writeMD5CryptBase64(&encoded, digest[3], digest[9], digest[15], 4)
	writeMD5CryptBase64(&encoded, digest[4], digest[10], digest[5], 4)
	writeMD5CryptBase64(&encoded, 0, 0, digest[11], 2)
	return encoded.String()
}

func writeMD5CryptBase64(output *strings.Builder, high, middle, low byte, count int) {
	value := uint(high)<<16 | uint(middle)<<8 | uint(low)
	for range count {
		output.WriteByte(md5CryptAlphabet[value&0x3f])
		value >>= 6
	}
}
