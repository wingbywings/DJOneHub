//go:build darwin && cgo
#import <AppKit/AppKit.h>
#import <CoreImage/CoreImage.h>
int dj_web_qr(const void *input,size_t length,void **output,size_t *out_length){
    @autoreleasepool {
        *output=NULL;*out_length=0;
        CIFilter *filter=[CIFilter filterWithName:@"CIQRCodeGenerator"];
        [filter setValue:[NSData dataWithBytes:input length:length] forKey:@"inputMessage"];
        [filter setValue:@"M" forKey:@"inputCorrectionLevel"];
        CIImage *image=[filter valueForKey:kCIOutputImageKey];if(!image)return 0;
        image=[image imageByApplyingTransform:CGAffineTransformMakeScale(6,6)];
        CIContext *context=[CIContext contextWithOptions:@{kCIContextUseSoftwareRenderer:@YES}];
        CGImageRef cg=[context createCGImage:image fromRect:[image extent]];if(!cg)return 0;
        NSBitmapImageRep *bitmap=[[NSBitmapImageRep alloc]initWithCGImage:cg];CGImageRelease(cg);
        NSData *png=[bitmap representationUsingType:NSBitmapImageFileTypePNG properties:@{}];[bitmap release];
        if(!png||[png length]>1048576)return 0;
        *output=malloc([png length]);if(!*output)return 0;
        memcpy(*output,[png bytes],[png length]);*out_length=[png length];return 1;
    }
}
