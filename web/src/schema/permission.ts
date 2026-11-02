// The wire types of pkg/schema/permission.go (ADR-014), generated into
// generated.ts: the permission catalog, the document GET and PUT
// /v1/permissions carry, and the live signal that it changed. Member links
// (invites v2) are here too, since the console lists them.
export {
  inviteV2,
  listInvitesResponseV2,
  inviteJoinersResponseV1,
  permission,
  permissionsResponseV1,
  putPermissionsRequestV1,
  type InviteJoinerV1,
  type InviteJoinersResponseV1,
  type InviteV2,
  type ListInvitesResponseV2,
  type Permission,
  type PermissionPreset,
  type PermissionPresetV1,
  type PermissionRowV1,
  type PermissionsResponseV1,
  type PermissionsWarning,
  type PutPermissionsRequestV1,
} from './generated';
