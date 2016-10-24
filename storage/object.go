package storage

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"git.letv.cn/yig/yig/api/datatype"
	. "git.letv.cn/yig/yig/error"
	"git.letv.cn/yig/yig/helper"
	"git.letv.cn/yig/yig/iam"
	"git.letv.cn/yig/yig/meta"
	"git.letv.cn/yig/yig/redis"
	"git.letv.cn/yig/yig/signature"
	"io"
	"math/rand"
	"time"
)

func (yig *YigStorage) pickCluster() (fsid string, err error) {
	var totalWeight int
	clusterWeights := make(map[string]int, len(yig.DataStorage))
	for fsid, _ := range yig.DataStorage {
		cluster, err := yig.MetaStorage.GetCluster(fsid)
		if err != nil {
			return "", err
		}
		totalWeight += cluster.Weight
		clusterWeights[fsid] = cluster.Weight
	}
	N := rand.Intn(totalWeight)
	n := 0
	for fsid, weight := range clusterWeights {
		n += weight
		if n > N {
			return fsid, nil
		}
	}
	return "", ErrInternalError
}

func (yig *YigStorage) PickOneClusterAndPool(bucket string, object string, size int64) (cluster *CephStorage,
	poolName string) {

	fsid, err := yig.pickCluster()
	if err != nil || fsid == "" {
		helper.Logger.Println("Error picking cluster:", err)
		for _, c := range yig.DataStorage {
			cluster = c
			break
		}
	} else {
		cluster = yig.DataStorage[fsid]
	}

	if size < 0 { // request.ContentLength is -1 if length is unknown
		poolName = BIG_FILE_POOLNAME
	}
	if size < BIG_FILE_THRESHOLD {
		poolName = SMALL_FILE_POOLNAME
	} else {
		poolName = BIG_FILE_POOLNAME
	}
	return
}

func (yig *YigStorage) GetObject(object *meta.Object, startOffset int64,
	length int64, writer io.Writer, sseRequest datatype.SseRequest) (err error) {

	var encryptionKey []byte
	if object.SseType == "S3" {
		encryptionKey = object.EncryptionKey
	} else { // SSE-C
		if len(sseRequest.CopySourceSseCustomerKey) != 0 {
			encryptionKey = sseRequest.CopySourceSseCustomerKey
		} else {
			encryptionKey = sseRequest.SseCustomerKey
		}
	}

	if len(object.Parts) == 0 { // this object has only one part
		cephCluster, ok := yig.DataStorage[object.Location]
		if !ok {
			return errors.New("Cannot find specified ceph cluster: " + object.Location)
		}
		getWholeObject := func(w io.Writer) error {
			err := cephCluster.get(object.Pool, object.ObjectId,
				0, object.Size, w)
			return err
		}

		if object.SseType == "" { // unencrypted object
			normalGet := func(w io.Writer) error {
				err := cephCluster.get(object.Pool, object.ObjectId,
					startOffset, length, w)
				return err
			}
			return yig.DataCache.WriteFromCache(object, startOffset, length, writer,
				normalGet, getWholeObject)
		}

		// encrypted object
		normalGet := func() (io.ReadCloser, error) {
			return cephCluster.getAlignedReader(object.Pool, object.ObjectId,
				startOffset, length)
		}
		reader, err := yig.DataCache.GetAlignedReader(object, startOffset, length, normalGet,
			getWholeObject)
		if err != nil {
			return err
		}
		defer reader.Close()

		decryptedReader, err := wrapAlignedEncryptionReader(reader, startOffset, encryptionKey,
			object.InitializationVector)
		if err != nil {
			return err
		}
		buffer := make([]byte, MAX_CHUNK_SIZE)
		_, err = io.CopyBuffer(writer, decryptedReader, buffer)
		return err
	}

	// multipart uploaded object
	for i := 1; i <= len(object.Parts); i++ {
		p := object.Parts[i]
		if p.Offset > startOffset+length {
			return
		}
		if p.Offset+p.Size >= startOffset {
			var readOffset, readLength int64
			if startOffset <= p.Offset {
				readOffset = 0
			} else {
				readOffset = startOffset - p.Offset
			}
			if p.Offset+p.Size <= startOffset+length {
				readLength = p.Offset + p.Size - readOffset
			} else {
				readLength = startOffset + length - (p.Offset + readOffset)
			}
			cephCluster, ok := yig.DataStorage[p.Location]
			if !ok {
				return errors.New("Cannot find specified ceph cluster: " +
					p.Location)
			}
			if object.SseType == "" { // unencrypted object
				err = cephCluster.get(p.Pool, p.ObjectId, readOffset, readLength, writer)
				if err != nil {
					return err
				}
				continue
			}

			reader, err := cephCluster.getAlignedReader(p.Pool, p.ObjectId,
				readOffset, length)
			if err != nil {
				return err
			}
			decryptedReader, err := wrapAlignedEncryptionReader(reader, readOffset,
				encryptionKey, p.InitializationVector)
			if err != nil {
				return err
			}
			buffer := make([]byte, MAX_CHUNK_SIZE)
			_, err = io.CopyBuffer(writer, decryptedReader, buffer)
			if err != nil {
				helper.Debugln("Multipart uploaded object write error:", err)
				return err
			}
			reader.Close()
		}
	}
	return
}

func (yig *YigStorage) GetObjectInfo(bucketName string, objectName string,
	version string, credential iam.Credential) (object *meta.Object, err error) {

	if version == "" {
		object, err = yig.MetaStorage.GetObject(bucketName, objectName)
	} else {
		object, err = yig.MetaStorage.GetObjectVersion(bucketName, objectName, version)
	}
	if err != nil {
		return
	}

	switch object.ACL.CannedAcl {
	case "public-read", "public-read-write":
		break
	case "authenticated-read":
		if credential.UserId == "" {
			err = ErrAccessDenied
			return
		}
	case "bucket-owner-read", "bucket-owner-full-control":
		bucket, err := yig.GetBucket(bucketName)
		if err != nil {
			return object, ErrAccessDenied
		}
		if bucket.OwnerId != credential.UserId {
			return object, ErrAccessDenied
		}
	default:
		if object.OwnerId != credential.UserId {
			err = ErrAccessDenied
			return
		}
	}

	return
}

func (yig *YigStorage) SetObjectAcl(bucketName string, objectName string, version string,
	acl datatype.Acl, credential iam.Credential) error {

	bucket, err := yig.MetaStorage.GetBucket(bucketName)
	if err != nil {
		return err
	}
	switch bucket.ACL.CannedAcl {
	case "bucket-owner-full-control":
		if bucket.OwnerId != credential.UserId {
			return ErrAccessDenied
		}
	default:
		if bucket.OwnerId != credential.UserId {
			return ErrAccessDenied
		}
	} // TODO policy and fancy ACL
	var object *meta.Object
	if version == "" {
		object, err = yig.MetaStorage.GetObject(bucketName, objectName)
	} else {
		object, err = yig.MetaStorage.GetObjectVersion(bucketName, objectName, version)
	}
	if err != nil {
		return err
	}
	object.ACL = acl
	err = yig.MetaStorage.PutObjectEntry(object)
	if err != nil {
		return err
	}
	if err == nil {
		yig.MetaStorage.Cache.Remove(redis.ObjectTable,
			bucketName+":"+objectName+":"+version)
	}
	return nil
}

// Write path:
//                                           +-----------+
// PUT object/part                           |           |   Ceph
//         +---------+------------+----------+ Encryptor +----->
//                   |            |          |           |
//                   |            |          +-----------+
//                   v            v
//                  SHA256      MD5(ETag)
//
// SHA256 is calculated only for v4 signed authentication
// Encryptor is enabled when user set SSE headers
func (yig *YigStorage) PutObject(bucketName string, objectName string, credential iam.Credential,
	size int64, data io.Reader, metadata map[string]string, acl datatype.Acl,
	sseRequest datatype.SseRequest) (result datatype.PutObjectResult, err error) {

	bucket, err := yig.MetaStorage.GetBucket(bucketName)
	if err != nil {
		return
	}

	switch bucket.ACL.CannedAcl {
	case "public-read-write":
		break
	default:
		if bucket.OwnerId != credential.UserId {
			return result, ErrBucketAccessForbidden
		}
	}

	md5Writer := md5.New()

	// Limit the reader to its provided size if specified.
	var limitedDataReader io.Reader
	if size > 0 { // request.ContentLength is -1 if length is unknown
		limitedDataReader = io.LimitReader(data, size)
	} else {
		limitedDataReader = data
	}

	cephCluster, poolName := yig.PickOneClusterAndPool(bucketName, objectName, size)

	// Mapping a shorter name for the object
	oid := cephCluster.GetUniqUploadName()
	dataReader := io.TeeReader(limitedDataReader, md5Writer)

	encryptionKey, err := encryptionKeyFromSseRequest(sseRequest)
	if err != nil {
		return
	}
	var initializationVector []byte
	if len(encryptionKey) != 0 {
		initializationVector, err = newInitializationVector()
		if err != nil {
			return
		}
	}
	storageReader, err := wrapEncryptionReader(dataReader, encryptionKey, initializationVector)
	if err != nil {
		return
	}
	bytesWritten, err := cephCluster.put(poolName, oid, storageReader)
	if err != nil {
		return
	}
	// Should metadata update failed, add `maybeObjectToRecycle` to `RecycleQueue`,
	// so the object in Ceph could be removed asynchronously
	maybeObjectToRecycle := objectToRecycle{
		location: cephCluster.Name,
		pool:     poolName,
		objectId: oid,
	}
	if bytesWritten < size {
		RecycleQueue <- maybeObjectToRecycle
		return result, ErrIncompleteBody
	}

	calculatedMd5 := hex.EncodeToString(md5Writer.Sum(nil))
	if userMd5, ok := metadata["md5Sum"]; ok {
		if userMd5 != "" && userMd5 != calculatedMd5 {
			RecycleQueue <- maybeObjectToRecycle
			return result, ErrBadDigest
		}
	}

	result.Md5 = calculatedMd5

	if signVerifyReader, ok := data.(*signature.SignVerifyReader); ok {
		credential, err = signVerifyReader.Verify()
		if err != nil {
			RecycleQueue <- maybeObjectToRecycle
			return
		}
	}

	// TODO validate bucket policy and fancy ACL

	object := &meta.Object{
		Name:             objectName,
		BucketName:       bucketName,
		Location:         cephCluster.Name,
		Pool:             poolName,
		OwnerId:          credential.UserId,
		Size:             bytesWritten,
		ObjectId:         oid,
		LastModifiedTime: time.Now().UTC(),
		Etag:             calculatedMd5,
		ContentType:      metadata["Content-Type"],
		ACL:              acl,
		NullVersion:      helper.Ternary(bucket.Versioning == "Enabled", false, true).(bool),
		DeleteMarker:     false,
		SseType:          sseRequest.Type,
		EncryptionKey: helper.Ternary(sseRequest.Type == "S3",
			encryptionKey, []byte("")).([]byte),
		InitializationVector: initializationVector,
		// TODO CustomAttributes
	}

	result.LastModified = object.LastModifiedTime

	switch bucket.Versioning {
	case "Enabled":
		result.VersionId = object.GetVersionId()
	case "Disabled":
		err = yig.removeObject(bucketName, objectName)
	case "Suspended":
		err = yig.removeNullVersionObject(bucketName, objectName)
	}
	if err != nil {
		RecycleQueue <- maybeObjectToRecycle
		return
	}

	err = yig.MetaStorage.PutObjectEntry(object)
	if err != nil {
		RecycleQueue <- maybeObjectToRecycle
		return
	}

	if err == nil {
		yig.MetaStorage.UpdateUsage(object.BucketName, object.Size)

		yig.MetaStorage.Cache.Remove(redis.ObjectTable, bucketName+":"+objectName+":")
		yig.DataCache.Remove(bucketName + ":" + objectName + ":" + object.GetVersionId())
	}
	return result, nil
}

func (yig *YigStorage) CopyObject(targetObject *meta.Object, source io.Reader, credential iam.Credential,
	sseRequest datatype.SseRequest) (result datatype.PutObjectResult, err error) {

	bucket, err := yig.MetaStorage.GetBucket(targetObject.BucketName)
	if err != nil {
		return
	}

	switch bucket.ACL.CannedAcl {
	case "public-read-write":
		break
	default:
		if bucket.OwnerId != credential.UserId {
			return result, ErrBucketAccessForbidden
		}
	}

	md5Writer := md5.New()

	// Limit the reader to its provided size if specified.
	var limitedDataReader io.Reader
	limitedDataReader = io.LimitReader(source, targetObject.Size)

	cephCluster, poolName := yig.PickOneClusterAndPool(targetObject.BucketName,
		targetObject.Name, targetObject.Size)

	// Mapping a shorter name for the object
	oid := cephCluster.GetUniqUploadName()
	dataReader := io.TeeReader(limitedDataReader, md5Writer)

	encryptionKey, err := encryptionKeyFromSseRequest(sseRequest)
	if err != nil {
		return
	}
	initializationVector, err := newInitializationVector()
	if err != nil {
		return
	}
	storageReader, err := wrapEncryptionReader(dataReader, encryptionKey, initializationVector)
	if err != nil {
		return
	}
	bytesWritten, err := cephCluster.put(poolName, oid, storageReader)
	if err != nil {
		return
	}
	// Should metadata update failed, add `maybeObjectToRecycle` to `RecycleQueue`,
	// so the object in Ceph could be removed asynchronously
	maybeObjectToRecycle := objectToRecycle{
		location: cephCluster.Name,
		pool:     poolName,
		objectId: oid,
	}
	if bytesWritten < targetObject.Size {
		RecycleQueue <- maybeObjectToRecycle
		return result, ErrIncompleteBody
	}

	calculatedMd5 := hex.EncodeToString(md5Writer.Sum(nil))
	if calculatedMd5 != targetObject.Etag {
		RecycleQueue <- maybeObjectToRecycle
		return result, ErrBadDigest
	}
	result.Md5 = calculatedMd5

	// TODO validate bucket policy and fancy ACL

	targetObject.Rowkey = ""    // clear the rowkey cache
	targetObject.VersionId = "" // clear the versionId cache
	targetObject.Location = cephCluster.Name
	targetObject.Pool = poolName
	targetObject.OwnerId = credential.UserId
	targetObject.ObjectId = oid
	targetObject.LastModifiedTime = time.Now().UTC()
	targetObject.NullVersion = helper.Ternary(bucket.Versioning == "Enabled", false, true).(bool)
	targetObject.DeleteMarker = false
	targetObject.SseType = sseRequest.Type
	targetObject.EncryptionKey = helper.Ternary(sseRequest.Type == "S3",
		encryptionKey, []byte("")).([]byte)
	targetObject.InitializationVector = initializationVector

	result.LastModified = targetObject.LastModifiedTime

	switch bucket.Versioning {
	case "Enabled":
		result.VersionId = targetObject.GetVersionId()
	case "Disabled":
		err = yig.removeObject(targetObject.BucketName, targetObject.Name)
	case "Suspended":
		err = yig.removeNullVersionObject(targetObject.BucketName, targetObject.Name)
	}
	if err != nil {
		RecycleQueue <- maybeObjectToRecycle
		return
	}

	err = yig.MetaStorage.PutObjectEntry(targetObject)
	if err != nil {
		RecycleQueue <- maybeObjectToRecycle
		return
	}

	if err == nil {
		yig.MetaStorage.UpdateUsage(targetObject.BucketName, targetObject.Size)

		yig.MetaStorage.Cache.Remove(redis.ObjectTable,
			targetObject.BucketName+":"+targetObject.Name+":")
		yig.DataCache.Remove(targetObject.BucketName + ":" + targetObject.Name + ":" + targetObject.GetVersionId())
	}
	return result, nil
}

func (yig *YigStorage) removeByObject(object *meta.Object) (err error) {
	err = yig.MetaStorage.DeleteObjectEntry(object)
	if err != nil {
		return
	}

	err = yig.MetaStorage.PutObjectToGarbageCollection(object)
	if err != nil { // try to rollback `objects` table
		yig.Logger.Println("Error PutObjectToGarbageCollection: ", err)
		err = yig.MetaStorage.PutObjectEntry(object)
		if err != nil {
			yig.Logger.Println("Error insertObjectEntry: ", err)
			yig.Logger.Println("Inconsistent data: object should be removed:",
				object)
			return
		}
		return ErrInternalError
	}

	yig.MetaStorage.UpdateUsage(object.BucketName, -object.Size)
	return nil
}

func (yig *YigStorage) removeObject(bucketName, objectName string) error {
	object, err := yig.MetaStorage.GetObject(bucketName, objectName)
	if err == ErrNoSuchKey {
		return nil
	}
	if err != nil {
		return err
	}
	return yig.removeByObject(object)
}

func (yig *YigStorage) removeObjectVersion(bucketName, objectName, version string) error {
	object, err := yig.MetaStorage.GetObjectVersion(bucketName, objectName, version)
	if err == ErrNoSuchKey {
		return nil
	}
	if err != nil {
		return err
	}
	return yig.removeByObject(object)
}

func (yig *YigStorage) removeNullVersionObject(bucketName, objectName string) error {
	object, err := yig.MetaStorage.GetNullVersionObject(bucketName, objectName)
	if err == ErrNoSuchVersion {
		return nil // When there's no null versioned object, we do not need to remove it
	}
	if err != nil {
		return err
	}
	return yig.removeByObject(object)
}

func (yig *YigStorage) addDeleteMarker(bucket meta.Bucket, objectName string) (versionId string, err error) {
	deleteMarker := &meta.Object{
		Name:             objectName,
		BucketName:       bucket.Name,
		OwnerId:          bucket.OwnerId,
		LastModifiedTime: time.Now().UTC(),
		NullVersion:      false,
		DeleteMarker:     true,
	}
	versionId = deleteMarker.GetVersionId()
	err = yig.MetaStorage.PutObjectEntry(deleteMarker)
	return
}

// When bucket versioning is Disabled/Enabled/Suspended, and request versionId is set/unset:
//
// |           |        with versionId        |                   without versionId                    |
// |-----------|------------------------------|--------------------------------------------------------|
// | Disabled  | error                        | remove object                                          |
// | Enabled   | remove corresponding version | add a delete marker                                    |
// | Suspended | remove corresponding version | remove null version(if exists) and add a delete marker |
//
// See http://docs.aws.amazon.com/AmazonS3/latest/dev/Versioning.html
func (yig *YigStorage) DeleteObject(bucketName string, objectName string, version string,
	credential iam.Credential) (result datatype.DeleteObjectResult, err error) {

	bucket, err := yig.MetaStorage.GetBucket(bucketName)
	if err != nil {
		return
	}
	switch bucket.ACL.CannedAcl {
	case "public-read-write":
		break
	default:
		if bucket.OwnerId != credential.UserId {
			return result, ErrBucketAccessForbidden
		}
	} // TODO policy and fancy ACL

	switch bucket.Versioning {
	case "Disabled":
		if version != "" {
			return result, ErrNoSuchVersion
		}
		err = yig.removeObject(bucketName, objectName)
		if err != nil {
			return
		}
	case "Enabled":
		if version == "" {
			result.VersionId, err = yig.addDeleteMarker(bucket, objectName)
			if err != nil {
				return
			}
			result.DeleteMarker = true
		} else {
			if version == "null" {
				err = yig.removeNullVersionObject(bucketName, objectName)
			} else {
				err = yig.removeObjectVersion(bucketName, objectName, version)
			}
			if err != nil {
				return
			}
			result.VersionId = version
		}
	case "Suspended":
		if version == "" {
			err = yig.removeNullVersionObject(bucketName, objectName)
			if err != nil {
				return
			}
			result.VersionId, err = yig.addDeleteMarker(bucket, objectName)
			if err != nil {
				return
			}
			result.DeleteMarker = true
		} else {
			if version == "null" {
				err = yig.removeNullVersionObject(bucketName, objectName)
			} else {
				err = yig.removeObjectVersion(bucketName, objectName, version)
			}
			if err != nil {
				return
			}
			result.VersionId = version
		}
	default:
		yig.Logger.Println("Invalid bucket versioning: ", bucketName)
		return result, ErrInternalError
	}

	if err == nil {
		yig.MetaStorage.Cache.Remove(redis.ObjectTable, bucketName+":"+objectName+":")
		yig.DataCache.Remove(bucketName + ":" + objectName + ":")
		yig.DataCache.Remove(bucketName + ":" + objectName + ":" + "null")
		if version != "" {
			yig.MetaStorage.Cache.Remove(redis.ObjectTable,
				bucketName+":"+objectName+":"+version)
			yig.DataCache.Remove(bucketName + ":" + objectName + ":" + version)
		}
	}
	return result, nil
}
