from typing import Literal

PublicAuthBlockMode = Literal["basic", "bearer", "open"]

PUBLIC_AUTH_BLOCK_MODE_VALUES: set[PublicAuthBlockMode] = {
    "basic",
    "bearer",
    "open",
}


def check_public_auth_block_mode(value: str) -> PublicAuthBlockMode:
    if value in PUBLIC_AUTH_BLOCK_MODE_VALUES:
        return value
    raise TypeError(f"Unexpected value {value!r}. Expected one of {PUBLIC_AUTH_BLOCK_MODE_VALUES!r}")
