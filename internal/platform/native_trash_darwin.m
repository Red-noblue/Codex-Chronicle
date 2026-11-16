// 中文说明：封装 Foundation 的卷感知系统回收站定位、移动 API 和错误转换。
#import <Foundation/Foundation.h>
#include <stdlib.h>
#include <string.h>
#include "native_trash_darwin.h"

static void cc_set_error(char **output, NSError *error, const char *fallback) {
    if (output == NULL) {
        return;
    }
    const char *message = error.localizedDescription.UTF8String;
    *output = strdup(message != NULL ? message : fallback);
}

static NSURL *cc_file_url(const char *path) {
    if (path == NULL) {
        return nil;
    }
    NSString *value = [[NSFileManager defaultManager] stringWithFileSystemRepresentation:path length:strlen(path)];
    if (value == nil) {
        return nil;
    }
    return [NSURL fileURLWithPath:value];
}

static int cc_copy_url_path(NSURL *url, char **output) {
    if (url == nil || output == NULL) {
        return -1;
    }
    const char *path = url.fileSystemRepresentation;
    if (path == NULL) {
        return -1;
    }
    *output = strdup(path);
    return *output == NULL ? -1 : 0;
}

static int cc_find_trash_directory(const char *item_path, char **trash_path, char **error_message, BOOL create) {
    if (trash_path != NULL) {
        *trash_path = NULL;
    }
    if (error_message != NULL) {
        *error_message = NULL;
    }
    @autoreleasepool {
        NSURL *item = cc_file_url(item_path);
        if (item == nil) {
            cc_set_error(error_message, nil, "源文件路径无法转换为 Foundation URL");
            return -1;
        }
        NSError *error = nil;
        NSURL *directory = [[NSFileManager defaultManager] URLForDirectory:NSTrashDirectory
                                                                  inDomain:NSUserDomainMask
                                                           appropriateForURL:item
                                                                      create:create
                                                                       error:&error];
        if (directory == nil) {
            cc_set_error(error_message, error, "Foundation 无法定位源文件所在卷的回收站");
            return -1;
        }
        if (cc_copy_url_path(directory, trash_path) != 0) {
            cc_set_error(error_message, nil, "读取 Foundation 回收站路径失败");
            return -1;
        }
        return 0;
    }
}

int cc_trash_directory(const char *item_path, char **trash_path, char **error_message) {
    return cc_find_trash_directory(item_path, trash_path, error_message, YES);
}

int cc_trash_directory_readonly(const char *item_path, char **trash_path, char **error_message) {
    return cc_find_trash_directory(item_path, trash_path, error_message, NO);
}

int cc_trash_item(const char *source_path, char **trash_path, char **error_message) {
    if (trash_path != NULL) {
        *trash_path = NULL;
    }
    if (error_message != NULL) {
        *error_message = NULL;
    }
    @autoreleasepool {
        NSURL *source = cc_file_url(source_path);
        if (source == nil) {
            cc_set_error(error_message, nil, "源文件路径无法转换为 Foundation URL");
            return -1;
        }
        NSError *error = nil;
        NSURL *result = nil;
        if (![[NSFileManager defaultManager] trashItemAtURL:source resultingItemURL:&result error:&error]) {
            cc_set_error(error_message, error, "Foundation 移入回收站失败");
            return -1;
        }
        if (cc_copy_url_path(result, trash_path) != 0) {
            cc_set_error(error_message, nil, "文件已移入回收站，但读取实际目标路径失败");
            return -1;
        }
        return 0;
    }
}
