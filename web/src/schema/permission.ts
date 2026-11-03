// The wire types of pkg/schema/permission.go (ADR-014), generated into
// generated.ts: the permission catalog, the document GET and PUT
// /v1/permissions carry, and the live signal that it changed. Member links
// (invites v2) are here too, since the console lists them. The permission
// vocabularies are client-tolerant (issue #1051): each type is any string,
// and Known… is the values this bundle has words for.
export {
  inviteV2,
  listInvitesResponseV2,
  inviteJoinersResponseV1,
  permission,
  permissionsResponseV1,
  putPermissionsRequestV1,
  type InviteJoinerV1,
  type IssuedByOperatorV1,
  type InviteJoinersResponseV1,
  type InviteV2,
  type KnownPermission,
  type KnownPermissionPreset,
  type ListInvitesResponseV2,
  type Permission,
  type PermissionChangeV1,
  type PermissionPreset,
  type PermissionPresetV1,
  type PermissionRowV1,
  type PermissionsResponseV1,
  type PermissionsWarning,
  type PutPermissionsRequestV1,
} from './generated';
