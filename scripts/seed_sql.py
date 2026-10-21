"""社区种子脚本共用的 SQL 文本生成工具。"""


def literal(value):
    """沿用种子脚本的字符串字面量规则，仅将单引号加倍。"""
    return "'" + value.replace("'", "''") + "'"
