//go:build darwin && cgo

#import <Foundation/Foundation.h>
#import <Security/Security.h>
#import <LocalAuthentication/LocalAuthentication.h>

int dj_remote_vault(int operation, const char *account, const void *data, size_t length, void **output, size_t *output_length) {
    @autoreleasepool {
        *output = NULL; *output_length = 0;
        NSString *name = [NSString stringWithUTF8String:account];
        if (!name) return errSecParam;
        LAContext *authentication = [[LAContext alloc] init];
        authentication.interactionNotAllowed = YES;
        NSMutableDictionary *query = [NSMutableDictionary dictionaryWithDictionary:@{
            (id)kSecClass: (id)kSecClassGenericPassword,
            (id)kSecAttrService: @"org.djonehub.remote.identity.v1",
            (id)kSecAttrAccount: name,
            (id)kSecAttrSynchronizable: @NO,
            (id)kSecUseDataProtectionKeychain: @NO,
            (id)kSecUseAuthenticationContext: authentication
        }];
        [authentication release];
        if (operation == 0) {
            query[(id)kSecReturnData] = @YES;
            query[(id)kSecMatchLimit] = (id)kSecMatchLimitOne;
            CFTypeRef result = NULL;
            OSStatus status = SecItemCopyMatching((CFDictionaryRef)query, &result);
            if (status != errSecSuccess) return status;
            if (!result || CFGetTypeID(result) != CFDataGetTypeID() || CFDataGetLength((CFDataRef)result) > 32768) {
                if (result) CFRelease(result);
                return errSecDecode;
            }
            CFIndex size = CFDataGetLength((CFDataRef)result);
            void *copy = malloc(size > 0 ? (size_t)size : 1);
            if (!copy) { CFRelease(result); return errSecAllocate; }
            if (size > 0) memcpy(copy, CFDataGetBytePtr((CFDataRef)result), (size_t)size);
            CFRelease(result);
            *output = copy; *output_length = (size_t)size;
            return errSecSuccess;
        }
        NSData *value = [NSData dataWithBytes:data length:length];
        if (operation == 1) {
            query[(id)kSecValueData] = value;
            query[(id)kSecAttrLabel] = @"DJOneHub remote Hub identity";
            return SecItemAdd((CFDictionaryRef)query, NULL);
        }
        if (operation == 2) {
            return SecItemUpdate((CFDictionaryRef)query, (CFDictionaryRef)@{(id)kSecValueData:value});
        }
        if (operation == 3) {
            return SecItemDelete((CFDictionaryRef)query);
        }
        return errSecParam;
    }
}
