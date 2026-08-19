/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * One row of the doctor report. Stable name tokens (dns_record / points_to_gregale / tls_certificate / caa_permits / ipv6_conflict) so the CLI can filter by name without parsing the human Detail field. Remediation is the exact record to change when status is fail — the load-bearing field for the activation drop-off.
 */
export type DomainDoctorCheck = {
  /**
   * Stable token — one of `dns_record` / `points_to_gregale` / `tls_certificate` / `caa_permits` / `ipv6_conflict`.
   */
  name: DomainDoctorCheck.name;
  /**
   * One of `ok` / `fail` / `pending` / `na`.
   */
  status: DomainDoctorCheck.status;
  /**
   * Human-readable description.
   */
  detail: string;
  /**
   * Raw observed value (CNAME target, AAAA record, CAA recordset, etc.).
   */
  observed?: string;
  /**
   * Exact record to change when status is fail — the load-bearing field for the activation drop-off.
   */
  remediation?: string;
  /**
   * RFC3339 time of this observation.
   */
  checked_at?: string;
};

export namespace DomainDoctorCheck {
  /**
   * Stable token — one of `dns_record` / `points_to_gregale` / `tls_certificate` / `caa_permits` / `ipv6_conflict`.
   */
  export enum name {
    DnsRecord = 'dns_record',
    PointsToGregale = 'points_to_gregale',
    TlsCertificate = 'tls_certificate',
    CaaPermits = 'caa_permits',
    Ipv6Conflict = 'ipv6_conflict',
  }
  /**
   * One of `ok` / `fail` / `pending` / `na`.
   */
  export enum status {
    Ok = 'ok',
    Fail = 'fail',
    Pending = 'pending',
    Na = 'na',
  }
}