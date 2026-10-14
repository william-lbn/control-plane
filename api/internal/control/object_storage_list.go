package control

// Shared grouped catalogue query: filter complete listing entries before limit.
// Common prefixes count once and never repeat when a cursor is inside a folder.
const storageListingSQL = `WITH entries AS (
 SELECT key,sha256,size,content_type,updated_at,
 CASE WHEN $4<>'' AND strpos(substring(key FROM char_length($2)+1),$4)>0
 THEN $2 || substring(substring(key FROM char_length($2)+1) FROM 1 FOR strpos(substring(key FROM char_length($2)+1),$4)+char_length($4)-1)
 ELSE key END AS listing_key,
 $4<>'' AND strpos(substring(key FROM char_length($2)+1),$4)>0 AS grouped
 FROM neon_storage.objects WHERE bucket=$1 AND starts_with(key,$2)
 ) SELECT listing_key AS key,bool_or(grouped) AS grouped,max(sha256) AS sha256,max(size) AS size,max(content_type) AS content_type,max(updated_at) AS updated_at
 FROM entries WHERE listing_key>$3 COLLATE "C" GROUP BY listing_key ORDER BY listing_key COLLATE "C" LIMIT $5`
